package datalayer

import (
	"context"
	"fmt"
	"strings"
	"sync"

	dshelp "github.com/ipfs/boxo/datastore/dshelp"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	query "github.com/ipfs/go-datastore/query"
	badger "github.com/ipfs/go-ds-badger2"
	format "github.com/ipfs/go-ipld-format"

	goJson "encoding/json"
)

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

			fmt.Println("put err", err)
		}
	}
	fmt.Println("Commiting batch")
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
		fmt.Println("Deleting entire key")
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
	fmt.Println("Fetching: ", blockCid.String())
	node, err := GC.fetcher.Get(ctx, blockCid)
	size, _ := node.Size()
	fmt.Println("Fetched", blockCid.String(), "at", size)

	if err != nil {
		return err
	}

	if recursive {
		links := make([]cid.Cid, 0)
		for _, v := range node.Links() {
			links = append(links, v.Cid)
			GC.AddPin(v.Cid, true)
		}
		err := GC.AddRef(blockCid, links)

		fmt.Println("AddRef err", err)
	}

	return nil
}

func (GC *GarbageCollector) RmPin(blockCid cid.Cid) error {

	ctx := context.Background()
	fmt.Println("Fetching (rm): ", blockCid.String())
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

		//Need to do more test work here to confirm its working ok
		fmt.Println(result.Key, result.Key)

		if result.Key == "" {
			break
		}

		splitted := strings.Split(result.Key, "/")
		mhd, _ := dshelp.DsKeyToMultihash(datastore.NewKey(splitted[2]))

		fmt.Println(mhd.B58String())

		links, _ := GC.GetRefs(cid.NewCidV0(mhd))

		if len(links) == 0 && pinMap[cid.NewCidV0(mhd)] != 1 {
			// fmt.Println("Looks, like you're a orphan. Delete! ")
			// fmt.Println(mhd.B58String())
			fmt.Println("Deleting: ", result.Key)
			batch.Delete(ctx, datastore.NewKey(result.Key))
		} else {
			// fmt.Println("You're popular!")
			// fmt.Println(mhd.B58String())
		}

		if !more {
			break
		}
	}
	batch.Commit(ctx)
	size, _ := GC.ds.DiskUsage(ctx)

	fmt.Println("Disk usage after", size)
	return nil
}

func NewGC(ds *badger.Datastore, fetcher format.DAGService) *GarbageCollector {

	return &GarbageCollector{
		ds:      ds,
		Ds:      ds,
		fetcher: fetcher,
	}
}
