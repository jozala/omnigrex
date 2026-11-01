package main

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type releaseVersion struct{ Minor, Patch, Alpha int64 }

func parseVersion(value string, alpha bool) (releaseVersion, error) {
	pattern := `^0\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`
	if alpha {
		pattern = `^0\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)-alpha\.([1-9][0-9]*)$`
	}
	matches := regexp.MustCompile(pattern).FindStringSubmatch(value)
	if matches == nil {
		return releaseVersion{}, errors.New("expected full pre-1.0 version without v prefix")
	}
	numbers := []int64{0, 0, 0}
	for i, s := range matches[1:] {
		n, e := strconv.ParseInt(s, 10, 64)
		if e != nil {
			return releaseVersion{}, errors.New("version component exceeds supported integer range")
		}
		numbers[i] = n
	}
	v := releaseVersion{numbers[0], numbers[1], numbers[2]}
	if v.Minor < 1 {
		return v, errors.New("first supported stable target is 0.1.0")
	}
	return v, nil
}
func (v releaseVersion) after(other releaseVersion) bool {
	return v.Minor > other.Minor || v.Minor == other.Minor && v.Patch > other.Patch
}
func progression(value string, alpha, breaking bool, releases []Release, tags []Tag) error {
	v, e := parseVersion(value, alpha)
	if e != nil {
		return e
	}
	for _, t := range tags {
		if t.Name == "v"+value {
			return errors.New("version already exists (including draft/tag)")
		}
	}
	previous := releaseVersion{}
	for _, r := range releases {
		if r.TagName == "v"+value {
			return errors.New("version already exists (including draft/tag)")
		}
		if !r.Draft && !r.Prerelease {
			old, e := parseVersion(strings.TrimPrefix(r.TagName, "v"), false)
			if e != nil {
				return fmt.Errorf("invalid existing stable release: %w", e)
			}
			if old.after(previous) {
				previous = old
			}
		}
	}
	if !v.after(previous) {
		return errors.New("stable target must move forward")
	}
	if breaking && previous.Minor != 0 && (v.Minor <= previous.Minor || v.Patch != 0) {
		return errors.New("breaking changes require a minor bump from previous stable")
	}
	if alpha {
		for _, r := range releases {
			old, e := parseVersion(strings.TrimPrefix(r.TagName, "v"), true)
			if e == nil && old.Minor == v.Minor && old.Patch == v.Patch && v.Alpha <= old.Alpha {
				return errors.New("alpha sequence must advance within target")
			}
		}
	}
	return nil
}
