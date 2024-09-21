package config

import (
	"encoding/json"
	"io"
	"os"
	"path"
	"reflect"
)

type Config[T any] struct {
	defaultValue T

	loaded bool
	value  T
}

const DATA_DIR = "data"
const CONFIG_DIR = DATA_DIR + "/config"

func New[T any](defaultValue T) *Config[T] {
	return &Config[T]{defaultValue: defaultValue}
}

func (c *Config[T]) filePath() string {
	name := reflect.TypeFor[T]().Name()
	return path.Join(CONFIG_DIR, name+".json")
}

func (c *Config[T]) Init() error {
	f, err := os.Open(c.filePath())
	if err != nil {
		if os.IsNotExist(err) {
			err = c.Update(func(t *T) {
				*t = c.defaultValue
			})
			if err != nil {
				return err
			}
		} else {
			return err
		}
	} else {
		defer f.Close()
		b, err := io.ReadAll(f)
		if err != nil {
			return err
		}
		err = json.Unmarshal(b, &c.value)
		if err != nil {
			return err
		}
	}
	c.loaded = true
	return nil
}

func (c *Config[T]) Start() error {
	return nil
}

func (c *Config[T]) Stop() error {
	return nil
}

func (c *Config[T]) Get() T {
	return c.value
}

func (c *Config[T]) Update(updater func(*T)) error {
	temp := c.value
	updater(&temp)
	b, err := json.MarshalIndent(temp, "", "  ")
	if err != nil {
		return err
	}
	err = os.MkdirAll(path.Dir(c.filePath()), 0755)
	if err != nil {
		return err
	}
	err = os.WriteFile(c.filePath(), b, 0644)
	if err != nil {
		return err
	}
	c.value = temp
	return nil
}
