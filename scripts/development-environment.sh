# Sourced by mise during tool installation and working-revision activation.
# Keep normal developer proxy settings when the image-local source is absent.
if [ -d /opt/omnigrex/go-proxy ]; then
  case "${GOPROXY:-}" in
    file:///opt/omnigrex/go-proxy|file:///opt/omnigrex/go-proxy,*) ;;
    *) export GOPROXY="file:///opt/omnigrex/go-proxy,${GOPROXY:-https://proxy.golang.org,direct}" ;;
  esac
fi
