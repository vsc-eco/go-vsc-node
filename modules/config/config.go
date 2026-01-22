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

	dataDir string
}

var UseMainConfigDuringTests = false

const DEFAULT_CONFIG_DIR string = "data/config"

func New[T any](defaultValue T, dataDir *string) *Config[T] {
	confDir := DEFAULT_CONFIG_DIR
	if dataDir != nil && *dataDir != "" {
		confDir = *dataDir + "/config"
	}

	return &Config[T]{
		defaultValue: defaultValue,
		dataDir:      confDir,
	}
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

func (c *Config[T]) FilePath() string {
	name := reflect.TypeFor[T]().Name()
	if testing.Testing() && UseMainConfigDuringTests && c.dataDir == DEFAULT_CONFIG_DIR {
		return path.Join(projectRoot().Expect("project root should be easy to find while running a test"), c.dataDir, name+".json")
	}
	return path.Join(c.dataDir, name+".json")
}

func (c *Config[T]) Init() error {
	f, err := os.Open(c.FilePath())
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
	err = os.MkdirAll(path.Dir(c.FilePath()), 0755)
	if err != nil {
		return err
	}
	err = os.WriteFile(c.FilePath(), b, 0644)
	if err != nil {
		return err
	}
	c.value = temp
	return nil
}

func (c *Config[T]) DefaultValue() T {
	return c.defaultValue
}
