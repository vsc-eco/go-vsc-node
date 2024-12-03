package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"vsc-node/lib/utils"

	"github.com/JustinKnueppel/go-result"
	"github.com/chebyrash/promise"
)

type Config[T any] struct {
	defaultValue T

	loaded bool
	value  T
}

var UseMainConfigDuringTests = true

const DATA_DIR = "data"
const CONFIG_DIR = DATA_DIR + "/config"

func New[T any](defaultValue T) *Config[T] {
	return &Config[T]{defaultValue: defaultValue}
}

func hasGoMod(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.Name() == "go.mod" {
			return true
		}
	}
	return false
}

func projectRoot() result.Result[string] {
	_, file, _, ok := runtime.Caller(0)
	wd := file
	if !ok {
		return result.Err[string](fmt.Errorf("could not find root caller source file"))
	}
	for !hasGoMod(wd) {
		prevWd := wd
		wd = filepath.Dir(wd)
		if wd == prevWd {
			return result.Err[string](fmt.Errorf("could not find root of project in this test. This is a bug with the test"))
		}
	}
	return result.Ok(wd)
}

func (c *Config[T]) filePath() string {
	name := reflect.TypeFor[T]().Name()
	if testing.Testing() && UseMainConfigDuringTests {
		return path.Join(projectRoot().Expect("project root should be easy to find while running a test"), CONFIG_DIR, name+".json")
	}
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

func (c *Config[T]) Start() *promise.Promise[any] {
	return utils.PromiseResolve[any](nil)
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
