package pi

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jozala/omnigrex/internal/agentprofile"
)

func (c *connection) configure(ctx context.Context) error {
	if c.options.RequireMCP {
		result, err := c.request(ctx, "get_commands", nil)
		if err != nil {
			return err
		}
		var commands struct {
			Commands []struct{ Name, Source string } `json:"commands"`
		}
		if json.Unmarshal(result.data, &commands) != nil {
			return ErrConfiguration
		}
		found := false
		budgetFound := false
		for _, command := range commands.Commands {
			if command.Name == "omnigrex-step-budget-v1" && command.Source == "extension" {
				budgetFound = true
			}
			if command.Name == "omnigrex-ready" && command.Source == "extension" {
				found = true
			}
		}
		if !found || c.options.StepBudget != nil && !budgetFound {
			return ErrConfiguration
		}
	}
	if c.options.Selection == nil {
		return nil
	}
	selection := *c.options.Selection
	provider, model, ok := strings.Cut(selection.Model, "/")
	if !ok || provider == "" || model == "" || strings.ContainsAny(selection.Model, " \t\r\n\x00") || selection.Thinking != "" && !agentprofile.ValidThinking(selection.Thinking) {
		return ErrConfiguration
	}
	result, err := c.request(ctx, "get_available_models", nil)
	if err != nil {
		return err
	}
	var available struct {
		Models []struct{ Provider, ID string } `json:"models"`
	}
	if json.Unmarshal(result.data, &available) != nil {
		return ErrConfiguration
	}
	found := false
	for _, candidate := range available.Models {
		if candidate.Provider == provider && candidate.ID == model {
			found = true
		}
	}
	if !found {
		return ErrConfiguration
	}
	if _, err := c.request(ctx, "set_model", map[string]string{"provider": provider, "modelId": model}); err != nil {
		return err
	}
	if selection.Thinking != "" {
		result, err = c.request(ctx, "get_available_thinking_levels", nil)
		if err != nil {
			return err
		}
		var available struct {
			Levels []string `json:"levels"`
		}
		if json.Unmarshal(result.data, &available) != nil {
			return ErrConfiguration
		}
		found = false
		for _, level := range available.Levels {
			if level == selection.Thinking {
				found = true
			}
		}
		if !found {
			return ErrConfiguration
		}
		if _, err := c.request(ctx, "set_thinking_level", map[string]string{"level": selection.Thinking}); err != nil {
			return err
		}
	}
	result, err = c.request(ctx, "get_state", nil)
	if err != nil {
		return err
	}
	var effective struct {
		Model    struct{ Provider, ID string } `json:"model"`
		Thinking string                        `json:"thinkingLevel"`
	}
	if json.Unmarshal(result.data, &effective) != nil || effective.Model.Provider != provider || effective.Model.ID != model || selection.Thinking != "" && effective.Thinking != selection.Thinking {
		return ErrConfiguration
	}
	return nil
}
