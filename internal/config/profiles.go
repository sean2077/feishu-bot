package config

import (
	"errors"
	"regexp"
	"strings"

	"github.com/sean2077/feishu-bot/internal/core"
)

var profileName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
var profileModel = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:/-]{0,199}$`)

func validEffort(value string) bool {
	switch value {
	case "", "auto", "low", "medium", "high", "xhigh", "max":
		return true
	}
	return false
}

func validateProfiles(c *core.Config) error {
	if !validEffort(c.Model.Effort) {
		return errors.New("model.effort must be auto, low, medium, high, xhigh or max")
	}
	if len(c.ModelProfiles) > 32 {
		return errors.New("modelProfiles supports at most 32 named profiles")
	}
	for name, profile := range c.ModelProfiles {
		if !profileName.MatchString(name) || name == "default" {
			return errors.New("modelProfiles names must be lowercase identifiers; default is reserved")
		}
		if !profileModel.MatchString(profile.Model) || (profile.SmallModel != "" && !profileModel.MatchString(profile.SmallModel)) {
			return errors.New("modelProfiles requires a native model identifier")
		}
		if !validEffort(profile.Effort) {
			return errors.New("modelProfiles effort must be auto, low, medium, high, xhigh or max")
		}
		if profile.SmallModel == "" {
			profile.SmallModel = profile.Model
		}
		c.ModelProfiles[name] = profile
	}
	return nil
}

// ErrModelProfileUnavailable reports a persisted session selection whose
// profile is no longer declared. Callers must not silently switch models.
var ErrModelProfileUnavailable = errors.New("selected model profile is unavailable; select a configured profile or default")

// ModelSelection reports the effective named model profile and where it came
// from. "default" is the deployment model; an explicitly selected "default"
// remains distinct from a session that has never selected a profile.
type ModelSelection struct {
	Profile string
	Source  string
}

// ResolveProjectModel returns the effective model config for one native
// process. An explicit session profile, including an explicit "default", wins;
// an empty session profile falls back to the project default, then to the
// deployment model. The returned config is a value copy.
func ResolveProjectModel(c core.Config, project core.Project, sessionProfile string) (core.Config, ModelSelection, error) {
	name, source := "", "default"
	if sessionProfile != "" {
		name, source = sessionProfile, "session"
	} else if project.ModelProfile != "" {
		name, source = project.ModelProfile, "project"
	}
	selected, err := ResolveSessionModel(c, name)
	if err != nil {
		return c, ModelSelection{}, err
	}
	if name == "" {
		name = "default"
	}
	return selected, ModelSelection{Profile: name, Source: source}, nil
}

// ResolveSessionModel returns a value copy. A worker must never mutate the
// factory's shared config when another conversation selects a profile.
func ResolveSessionModel(c core.Config, name string) (core.Config, error) {
	if name == "" || name == "default" {
		return c, nil
	}
	profile, ok := c.ModelProfiles[name]
	if !ok || strings.TrimSpace(profile.Model) == "" {
		return c, ErrModelProfileUnavailable
	}
	c.Model.Model = profile.Model
	c.Model.SmallModel = profile.SmallModel
	if c.Model.SmallModel == "" {
		c.Model.SmallModel = profile.Model
	}
	if profile.Effort != "" {
		c.Model.Effort = profile.Effort
	}
	return c, nil
}
