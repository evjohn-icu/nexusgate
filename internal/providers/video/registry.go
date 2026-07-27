package video

import (
	"fmt"
	"sort"
)

type Registry struct {
	providers map[string]VideoUnderstandingProvider
}

func NewRegistry() *Registry {
	return &Registry{providers: make(map[string]VideoUnderstandingProvider)}
}

func (r *Registry) Register(provider VideoUnderstandingProvider) error {
	if provider == nil || provider.Name() == "" {
		return fmt.Errorf("video provider must have a name")
	}
	if r.providers == nil {
		r.providers = make(map[string]VideoUnderstandingProvider)
	}
	r.providers[provider.Name()] = provider
	return nil
}

func (r *Registry) Get(name string) (VideoUnderstandingProvider, bool) {
	provider, ok := r.providers[name]
	return provider, ok
}

func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.providers))
	for name := range r.providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
