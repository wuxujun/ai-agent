package promptmanager

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

type VersionPin struct {
	Name     string   `json:"name"`
	Version  int      `json:"version"`
	Selector Selector `json:"selector"`
	Labels   []string `json:"labels,omitempty"`
}

type VersionPinRegistry struct {
	mu    sync.RWMutex
	pins  map[string]VersionPin
	onPin func(VersionPin)
}

func NewVersionPinRegistry(initial []VersionPin, onPin func(VersionPin)) (*VersionPinRegistry, error) {
	registry := &VersionPinRegistry{pins: make(map[string]VersionPin, len(initial)), onPin: onPin}
	for _, pin := range initial {
		normalized, err := normalizeVersionPin(pin)
		if err != nil {
			return nil, err
		}
		if existing, ok := registry.pins[normalized.Name]; ok && existing.Version != normalized.Version {
			return nil, fmt.Errorf("conflicting prompt version pins for %q: %d and %d", normalized.Name, existing.Version, normalized.Version)
		}
		registry.pins[normalized.Name] = normalized
	}
	return registry, nil
}

func (r *VersionPinRegistry) Get(name string) (VersionPin, bool) {
	if r == nil {
		return VersionPin{}, false
	}
	name = strings.TrimSpace(name)
	r.mu.RLock()
	pin, ok := r.pins[name]
	r.mu.RUnlock()
	if !ok {
		return VersionPin{}, false
	}
	return cloneVersionPin(pin), true
}

func (r *VersionPinRegistry) Pin(pin VersionPin) error {
	if r == nil {
		return nil
	}
	normalized, err := normalizeVersionPin(pin)
	if err != nil {
		return err
	}
	r.mu.Lock()
	if existing, ok := r.pins[normalized.Name]; ok {
		r.mu.Unlock()
		if existing.Version != normalized.Version {
			return fmt.Errorf("prompt %q is pinned to version %d, resolved version %d", normalized.Name, existing.Version, normalized.Version)
		}
		return nil
	}
	r.pins[normalized.Name] = normalized
	onPin := r.onPin
	r.mu.Unlock()
	if onPin != nil {
		onPin(cloneVersionPin(normalized))
	}
	return nil
}

func normalizeVersionPin(pin VersionPin) (VersionPin, error) {
	pin.Name = strings.TrimSpace(pin.Name)
	if pin.Name == "" {
		return VersionPin{}, fmt.Errorf("prompt version pin requires a name")
	}
	if pin.Version <= 0 {
		return VersionPin{}, fmt.Errorf("prompt version pin for %q requires a positive version", pin.Name)
	}
	selector, err := pin.Selector.Normalize()
	if err != nil {
		return VersionPin{}, err
	}
	pin.Selector = selector
	pin.Labels = append([]string(nil), pin.Labels...)
	return pin, nil
}

func cloneVersionPin(pin VersionPin) VersionPin {
	pin.Labels = append([]string(nil), pin.Labels...)
	return pin
}

type versionPinRegistryContextKey struct{}

func WithVersionPinRegistry(ctx context.Context, registry *VersionPinRegistry) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, versionPinRegistryContextKey{}, registry)
}

func versionPinRegistryFromContext(ctx context.Context) *VersionPinRegistry {
	if ctx == nil {
		return nil
	}
	registry, _ := ctx.Value(versionPinRegistryContextKey{}).(*VersionPinRegistry)
	return registry
}
