//go:build integration

package deployment

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWebsitePublicationWithRestrictedSSH(t *testing.T) {
	root := repositoryRoot(t)
	deploy, err := os.ReadFile(filepath.Join(root, "website/scripts/deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	id := time.Now().UnixNano()
	image := fmt.Sprintf("omnigrex/website-rsync-test:%d", id)
	container := fmt.Sprintf("omnigrex-website-rsync-test-%d", id)
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "--force", container).Run()
		_ = exec.Command("docker", "image", "rm", image).Run()
	})
	// The production host uses Ubuntu 24.04's standard rsync and rrsync.
	// All keys and files below are disposable container-local fixtures.
	dir := t.TempDir()
	dockerfile := `FROM ubuntu:24.04
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends openssh-server rsync python3 && rm -rf /var/lib/apt/lists/*
`
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("docker", "build", "--tag", image, dir)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build restricted SSH fixture: %v\n%s", err, output)
	}
	fixture := `set -eu
mkdir -p /fixture/source/docs /fixture/runner /srv/website /srv/unrelated /var/lib/website-deploy/.ssh /run/sshd
useradd --system --user-group --home-dir /var/lib/website-deploy --shell /bin/dash --password '*' website-deploy
chmod 0755 /var/lib/website-deploy /var/lib/website-deploy/.ssh
chown website-deploy:website-deploy /srv/website
printf 'keep outside website\n' > /srv/unrelated/keep.txt
printf 'obsolete\n' > /srv/website/obsolete.html
printf 'home must survive\n' > /var/lib/website-deploy/keep.txt
printf 'home page\n' > /fixture/source/index.html
printf 'documentation\n' > /fixture/source/docs/index.html
ln -s /srv/unrelated /fixture/source/outside-link
ssh-keygen -q -t ed25519 -N '' -f /fixture/client_key
ssh-keygen -q -t ed25519 -N '' -f /fixture/host_key
printf 'restrict,command="/usr/bin/rrsync -wo -munge /srv/website" %s\n' "$(cat /fixture/client_key.pub)" > /var/lib/website-deploy/.ssh/authorized_keys
chmod 0644 /var/lib/website-deploy/.ssh/authorized_keys
cp /var/lib/website-deploy/.ssh/authorized_keys /fixture/authorized_keys.expected
cat > /fixture/sshd_config <<'SSH_CONFIG'
Port 2222
ListenAddress 127.0.0.1
HostKey /fixture/host_key
PidFile /fixture/sshd.pid
PasswordAuthentication no
KbdInteractiveAuthentication no
UsePAM no
StrictModes yes
AllowUsers website-deploy
AuthorizedKeysFile .ssh/authorized_keys
SSH_CONFIG
/usr/sbin/sshd -f /fixture/sshd_config
cat > /fixture/deploy.sh <<'WEBSITE_DEPLOY_SCRIPT'
@DEPLOY_SCRIPT@
WEBSITE_DEPLOY_SCRIPT
export RUNNER_TEMP=/fixture/runner
export WEBSITE_SSH_HOST=127.0.0.1 WEBSITE_SSH_PORT=2222 WEBSITE_SSH_USER=website-deploy
export WEBSITE_SSH_KEY="$(cat /fixture/client_key)"
export WEBSITE_SSH_KNOWN_HOSTS="[127.0.0.1]:2222 $(cat /fixture/host_key.pub)"
# A stale, dangerous GitHub variable must not redirect the upload.
export WEBSITE_DEPLOY_PATH=/var/lib/website-deploy
sh /fixture/deploy.sh /fixture/source
cmp /fixture/source/index.html /srv/website/index.html
cmp /fixture/source/docs/index.html /srv/website/docs/index.html
test ! -e /srv/website/obsolete.html
test "$(readlink /srv/website/outside-link)" = /rsyncd-munged//srv/unrelated
test "$(cat /srv/unrelated/keep.txt)" = 'keep outside website'
test "$(cat /var/lib/website-deploy/keep.txt)" = 'home must survive'
cmp /fixture/authorized_keys.expected /var/lib/website-deploy/.ssh/authorized_keys
test -z "$(ls -A /fixture/runner)"
printf '%s\n' "$WEBSITE_SSH_KNOWN_HOSTS" > /fixture/known_hosts
transport='ssh -F /dev/null -i /fixture/client_key -p 2222 -o BatchMode=yes -o IdentitiesOnly=yes -o IdentityAgent=none -o StrictHostKeyChecking=yes -o UserKnownHostsFile=/fixture/known_hosts'
if $transport website-deploy@127.0.0.1 id > /fixture/command.log 2>&1; then
  echo 'arbitrary SSH command unexpectedly allowed' >&2
  exit 1
fi
grep -F 'SSH_ORIGINAL_COMMAND does not run rsync' /fixture/command.log
if rsync -rni -e "$transport" /fixture/source/ website-deploy@127.0.0.1:../unrelated/ > /fixture/escape.log 2>&1; then
  echo 'directory escape unexpectedly allowed' >&2
  exit 1
fi
grep -F 'do not use ..' /fixture/escape.log
if rsync -rni -e "$transport" website-deploy@127.0.0.1:./ /fixture/download/ > /fixture/download.log 2>&1; then
  echo 'download unexpectedly allowed' >&2
  exit 1
fi
grep -F 'reading from write-only server is not allowed' /fixture/download.log
`
	run := exec.Command("docker", "run", "--rm", "--interactive", "--name", container, image, "sh", "-s")
	run.Stdin = strings.NewReader(strings.ReplaceAll(fixture, "@DEPLOY_SCRIPT@", string(deploy)))
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("restricted SSH publication: %v\n%s", err, output)
	}
}
