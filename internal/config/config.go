package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Action string

const (
	ActionTrash   Action = "trash"
	ActionDelete  Action = "delete"
	ActionArchive Action = "archive"
	ActionLabel   Action = "label"
)

const defaultMaxPerRun = 500

type Rule struct {
	Name      string `yaml:"name"`
	Query     string `yaml:"query"`
	Action    Action `yaml:"action"`
	Label     string `yaml:"label,omitempty"`
	MaxPerRun int    `yaml:"max_per_run,omitempty"`
}

type File struct {
	Rules []Rule `yaml:"rules"`
}

func Load(path string) (*File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var f File
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := f.validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

func (f *File) validate() error {
	if len(f.Rules) == 0 {
		return fmt.Errorf("no rules defined")
	}
	seen := make(map[string]struct{}, len(f.Rules))
	for i := range f.Rules {
		r := &f.Rules[i]
		if r.Name == "" {
			return fmt.Errorf("rule[%d]: name is required", i)
		}
		if _, dup := seen[r.Name]; dup {
			return fmt.Errorf("duplicate rule name: %q", r.Name)
		}
		seen[r.Name] = struct{}{}

		if r.Query == "" {
			return fmt.Errorf("rule %q: query is required", r.Name)
		}
		switch r.Action {
		case ActionTrash, ActionDelete, ActionArchive, ActionLabel:
		case "":
			return fmt.Errorf("rule %q: action is required", r.Name)
		default:
			return fmt.Errorf("rule %q: unknown action %q (want: trash | delete | archive | label)", r.Name, r.Action)
		}
		if r.Action == ActionLabel && r.Label == "" {
			return fmt.Errorf("rule %q: label is required when action is 'label'", r.Name)
		}
		if r.MaxPerRun < 0 {
			return fmt.Errorf("rule %q: max_per_run must be >= 0", r.Name)
		}
		if r.MaxPerRun == 0 {
			r.MaxPerRun = defaultMaxPerRun
		}
	}
	return nil
}

// Find returns the rule with the given name, or nil.
func (f *File) Find(name string) *Rule {
	for i := range f.Rules {
		if f.Rules[i].Name == name {
			return &f.Rules[i]
		}
	}
	return nil
}
