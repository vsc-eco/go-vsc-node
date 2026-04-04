package devnet

import (
	"embed"
	"os"
	"path/filepath"
)

//go:embed testdata/config.ini testdata/pgtune.conf
var testdataFS embed.FS

// createHAFDataDirs creates the directory structure required by a HAF
// testnet container. It also copies the embedded config.ini and
// pgtune.conf into the correct locations.
func createHAFDataDirs(hafDataDir string) error {
	dirs := []string{
		"blockchain",
		"haf_db_store/pgdata/pg_wal",
		"haf_db_store/tablespace",
		"haf_db_store/haf_postgresql_conf.d",
		"logs/postgresql",
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(hafDataDir, d), 0o755); err != nil {
			return err
		}
	}

	configIni, err := testdataFS.ReadFile("testdata/config.ini")
	if err != nil {
		return err
	}
	if err := os.WriteFile(
		filepath.Join(hafDataDir, "config.ini"),
		configIni, 0o644,
	); err != nil {
		return err
	}

	pgtune, err := testdataFS.ReadFile("testdata/pgtune.conf")
	if err != nil {
		return err
	}
	return os.WriteFile(
		filepath.Join(hafDataDir, "haf_db_store", "haf_postgresql_conf.d", "pgtune.conf"),
		pgtune, 0o644,
	)
}
