package database

import (
	"bytes"
	"context"
	"encoding/base32"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/ipfs/go-datastore"
	flatfs "github.com/ipfs/go-ds-flatfs"
)

var (
	ErrAddrExists   = errors.New("key exists in datastore")
	ErrAddrNotFound = errors.New("address not found")
)

type MappingBotDatabase struct {
	db  *flatfs.Datastore
	mtx *sync.Mutex
}

func New(path string) (*MappingBotDatabase, error) {
	if err := os.MkdirAll(path, 0755); err != nil {
		return nil, err
	}

	// uses default sharding
	fs, err := flatfs.CreateOrOpen(path, flatfs.NextToLast(2), false)
	if err != nil {
		return nil, err
	}

	mdb := &MappingBotDatabase{
		db:  fs,
		mtx: &sync.Mutex{},
	}

	return mdb, nil
}

// store key in datastore, returns any operational error, or ErrAddrExists if
// key exists in datastore.
func (m *MappingBotDatabase) InsertAddressMap(
	ctx context.Context,
	btcAddr, vscAddr string,
) error {
	key, err := makeFlatFsKey(btcAddr)
	if err != nil {
		return fmt.Errorf(
			"failed to make flat fs key [btcAddr:%s]: %w",
			btcAddr, err,
		)
	}

	m.mtx.Lock()
	defer m.mtx.Unlock()

	// checks for address existence
	exists, err := m.db.Has(ctx, *key)
	if err != nil {
		return fmt.Errorf("failed to query key [%s]: %w", btcAddr, err)
	}

	if exists {
		return ErrAddrExists
	}

	// value encoding
	if err := m.db.Put(ctx, *key, []byte(vscAddr)); err != nil {
		return fmt.Errorf("failed to put key [%s]: %w", btcAddr, err)
	}

	return nil
}

// return vsc address from btcAddr, if vsc address not found, returns
// ErrAddrNotFound, otherwise any operational error
func (m *MappingBotDatabase) GetVscAddress(
	ctx context.Context,
	btcAddr string,
) (string, error) {
	key, err := makeFlatFsKey(btcAddr)
	if err != nil {
		return "", fmt.Errorf(
			"failed to make flat fs key [btcAddr:%s]: %w",
			btcAddr, err,
		)
	}

	m.mtx.Lock()
	defer m.mtx.Unlock()

	vscAddr, err := m.db.Get(ctx, *key)
	if err != nil {
		if errors.Is(err, datastore.ErrNotFound) {
			return "", ErrAddrNotFound
		}

		return "", fmt.Errorf(
			"failed to get vscAddress [btcAddr: %s]: %w",
			btcAddr, err,
		)
	}

	return string(vscAddr), nil
}

func makeFlatFsKey(k string) (*datastore.Key, error) {
	buf := &bytes.Buffer{}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding)

	encoder := base32.NewEncoder(enc, buf)
	if _, err := encoder.Write([]byte(k)); err != nil {
		return nil, err
	}
	encoder.Close()

	datastoreKey := datastore.NewKey(buf.String())

	return &datastoreKey, nil
}
