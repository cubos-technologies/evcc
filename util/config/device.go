package config

import (
	"reflect"

	"github.com/evcc-io/evcc/core/loadpoint"
	"github.com/evcc-io/evcc/util/templates"
)

type Device[T any] interface {
	Config() Named
	Instance() T
}
type ConfigurableDevice[T any] interface {
	Device[T]
	ID() int
	Update(map[string]any, T) error
	Delete() error
}

type configurableDevice[T any] struct {
	config   Config
	instance T
}

func NewConfigurableDevice[T any](config Config, instance T) ConfigurableDevice[T] {
	return &configurableDevice[T]{
		config:   config,
		instance: instance,
	}
}

func BlankConfigurableDevice[T any]() ConfigurableDevice[T] {
	// NOTE: creating loadpoint will read from settings, hence config.Value must be valid json
	//var value T
	var class templates.Class
	t := reflect.TypeOf(new(T))
	f := reflect.TypeOf((*loadpoint.API)(nil))
	switch t {
	case f:
		class = templates.Loadpoint
	default:
		class = 0
	}

	return &configurableDevice[T]{
		config: Config{
			Value: "{}",
			Class: class,
		},
	}
}

func (d *configurableDevice[T]) Config() Named {
	return d.config.Named()
}

func (d *configurableDevice[T]) Instance() T {
	return d.instance
}

func (d *configurableDevice[T]) ID() int {
	return d.config.ID
}

func (d *configurableDevice[T]) Update(config map[string]any, instance T) error {
	if err := d.config.Read(config); err != nil {
		if err := d.config.Update(config); err != nil {
			return err
		}
	}
	d.instance = instance
	return nil
}

func (d *configurableDevice[T]) Delete() error {
	return d.config.Delete()
}

type staticDevice[T any] struct {
	config   Named
	instance T
}

func NewStaticDevice[T any](config Named, instance T) Device[T] {
	return &staticDevice[T]{
		config:   config,
		instance: instance,
	}
}

func (d *staticDevice[T]) Configurable() bool {
	return true
}

func (d *staticDevice[T]) Config() Named {
	return d.config
}

func (d *staticDevice[T]) Instance() T {
	return d.instance
}
