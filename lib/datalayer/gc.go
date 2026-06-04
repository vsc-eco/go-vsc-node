package datalayer

import (
	"context"
	"strings"
	"sync"
	"vsc-node/lib/vsclog"

	dshelp "github.com/ipfs/boxo/datastore/dshelp"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	query "github.com/ipfs/go-datastore/query"
	badger "github.com/ipfs/go-ds-badger2"
	format "github.com/ipfs/go-ipld-format"

	goJson "encoding/json"
)

var log = vsclog.Module("datalayer")

// If you're reading this, it probably means you want to know what this is.
// This is a reverse mapping from child -> parent CIDs to speed up garbage collection
// It maintains a lightweight list of parents so deduplication can be done with reading minimal data.
type GarbageCollector struct {
	ds      *badger.Datastore
	Ds      *badger.Datastore
	fetcher format.DAGService

	mutex sync.Mutex
}

type LinkType struct {
	refs []string
}

type PinType struct {
	cid       cid.Cid
	recursive string
}

// Cid of parent
// Refs (children) of parents
func (GC *GarbageCollector) AddRef(parentCid cid.Cid, refs []cid.Cid) error {
	GC.mutex.Lock()
	defer GC.mutex.Unlock()
	ctx := context.Background()

	batch, err := GC.ds.Batch(ctx)
	if err != nil {
		return err
	}
	//Process each child from child -> parent relationship
	for _, ref := range refs {
		//Make sure to use hash of CID not CID as it's only the content hash and does not include headers.
		key := datastore.NewKey(`/gc-links/` + ref.Hash().B58String())
		linkData, err := GC.ds.Get(ctx, key)

		decodedData := make(map[string]int)
		if err == datastore.ErrNotFound {
			//Dont do anything
		} else if err != nil {
			return err
		} else {
			err := goJson.Unmarshal(linkData, &decodedData)
			if err != nil {
				return err
			}
		}

		if decodedData[parentCid.Hash().B58String()] != 1 {
			decodedData[parentCid.Hash().B58String()] = 1
			editedBytes, err := goJson.Marshal(decodedData)
			if err != nil {
				return err
			}
			err = batch.Put(ctx, key, editedBytes)
			if err != nil {
				log.Trace("AddRef: batch.Put error", "err", err)
			}
		}
	}
	log.Trace("AddRef: committing batch")
	err = batch.Commit(ctx)

	if err != nil {
		return err
	}

	return nil
}

func (GC *GarbageCollector) RmRef(parentCid cid.Cid, child cid.Cid, batch datastore.Batch) error {
	ctx := context.Background()
	key := datastore.NewKey("/gc-links/" + child.Hash().B58String())

	linkData, err := GC.ds.Get(ctx, key)
	decodedData := make(map[string]int)

	if err != nil && err != datastore.ErrNotFound {
		return err
	} else {
		err := goJson.Unmarshal(linkData, &decodedData)
		if err != nil {
			return err
		}
	}

	if decodedData[parentCid.Hash().B58String()] == 1 {
		log.Trace("RmRef: deleting parent key")
		delete(decodedData, parentCid.Hash().B58String())
	}

	if len(decodedData) == 0 {
		return batch.Delete(ctx, key)
	} else {
		//Serialize and store as such.
		bytes, err := goJson.Marshal(decodedData)
		if err != nil {
			return err
		}

		return batch.Put(ctx, key, bytes)
	}
}

func (GC *GarbageCollector) GetRefs(cid cid.Cid) ([]string, error) {
	ctx := context.Background()
	key := datastore.NewKey("/gc-links/" + cid.Hash().B58String())

	bytes, err := GC.ds.Get(ctx, key)

	if err != nil {
		return nil, err
	} else if err == datastore.ErrNotFound {
		return make([]string, 0), nil
	} else {
		//Everthing is ok
		dataVal := make(map[string]int)
		goJson.Unmarshal(bytes, &dataVal)
		//Collect keys

		out := make([]string, 0)
		for key := range dataVal {
			out = append(out, key)
		}
		return out, nil
	}
}

func (GC *GarbageCollector) AddPin(blockCid cid.Cid, recursive bool) error {
	ctx := context.Background()
	log.Trace("AddPin: fetching", "cid", blockCid.String())
	node, err := GC.fetcher.Get(ctx, blockCid)
	size, _ := node.Size()
	log.Trace("AddPin: fetched", "cid", blockCid.String(), "size", size)

	if err != nil {
		return err
	}

	if recursive {
		links := make([]cid.Cid, 0)
		for _, v := range node.Links() {
			links = append(links, v.Cid)
			GC.AddPin(v.Cid, true)
		}
		if err := GC.AddRef(blockCid, links); err != nil {
			log.Trace("AddPin: AddRef error", "err", err)
		}
	}

	return nil
}

func (GC *GarbageCollector) RmPin(blockCid cid.Cid) error {

	ctx := context.Background()
	log.Trace("RmPin: fetching", "cid", blockCid.String())
	node, err := GC.fetcher.Get(ctx, blockCid)

	if err != nil {
		return nil
	}
	batch, _ := GC.ds.Batch(ctx)
	//Remove self reference from child pin
	for _, v := range node.Links() {
		// b58 := v.Cid.Hash().B58String()
		GC.RmRef(blockCid, v.Cid, batch)
		// links,_ := GC.GetRefs(v.Cid)
	}
	batch.Commit(ctx)

	for _, v := range node.Links() {
		links, _ := GC.GetRefs(v.Cid)

		if len(links) == 0 {
			//Should deref children as there is no parent to this node.
			GC.RmPin(v.Cid)
		}
	}

	return nil
}

func (GC *GarbageCollector) GC(pinList []cid.Cid) error {
	//Convert pinlist to map and convert to mulithash aka CID v0
	pinMap := make(map[cid.Cid]int)
	for _, v := range pinList {
		pinMap[cid.NewCidV0(v.Hash())] = 1
	}
	ctx := context.Background()

	results, err := GC.ds.Query(ctx, query.Query{
		Prefix: "/blocks/",
		//We only need keys, dont look for more.
		KeysOnly: true,
	})

	if err != nil {
		return err
	}

	batch, _ := GC.ds.Batch(ctx)
	for {
		result, more := results.NextSync()

		log.Trace("GC: scan key", "key", result.Key)

		if result.Key == "" {
			break
		}

		splitted := strings.Split(result.Key, "/")
		mhd, _ := dshelp.DsKeyToMultihash(datastore.NewKey(splitted[2]))

		log.Trace("GC: multihash", "b58", mhd.B58String())

		links, _ := GC.GetRefs(cid.NewCidV0(mhd))

		if len(links) == 0 && pinMap[cid.NewCidV0(mhd)] != 1 {
			log.Trace("GC: deleting orphan", "key", result.Key)
			batch.Delete(ctx, datastore.NewKey(result.Key))
		}

		if !more {
			break
		}
	}
	batch.Commit(ctx)
	size, _ := GC.ds.DiskUsage(ctx)

	log.Verbose("GC: disk usage after", "size", size)
	return nil
}

func NewGC(ds *badger.Datastore, fetcher format.DAGService) *GarbageCollector {

	return &GarbageCollector{
		ds:      ds,
		Ds:      ds,
		fetcher: fetcher,
	}
}
