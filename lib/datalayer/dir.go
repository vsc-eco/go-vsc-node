package datalayer

import (
	"context"
	"errors"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	uio "github.com/ipfs/boxo/ipld/unixfs/io"
	"github.com/ipfs/go-cid"
	ipld "github.com/ipfs/go-ipld-format"
	"github.com/multiformats/go-multicodec"
)

// materializeGetNode forces all HAMT children into memory before serializing,
// ensuring deterministic CID computation. HAMTDirectory.ForEachLink uses
// walkTrie → childer.each → childer.get → loadChild, which populates the
// in-memory children array. After this, Shard.Node() uniformly takes the
// "loaded child" code path for every slot, eliminating the two-path
// serialization divergence that causes non-deterministic CIDs.
// For BasicDirectory this is a cheap no-op iteration over the flat link list.
func materializeGetNode(dir uio.Directory) (ipld.Node, error) {
	dir.ForEachLink(context.Background(), func(*ipld.Link) error { return nil })
	return dir.GetNode()
}

type MutableDirectory struct {
	uio.DynamicDirectory
}

type LeafDir struct {
	Dir         uio.Directory
	leaves      map[string]*LeafDir
	leafEntries map[string]cid.Cid // leaf key → CID cache; avoids Dir.Find blockstore reads
	leafDeleted bool
}

// Applies leaves to Dir
func (lf *LeafDir) Compact(recursive bool) {
	// Sort leaf keys for deterministic iteration order.
	// BasicDirectory sorts links on encode, but consistent insertion order
	// avoids any edge cases in the underlying directory implementation.
	leafKeys := make([]string, 0, len(lf.leaves))
	for key := range lf.leaves {
		leafKeys = append(leafKeys, key)
	}
	sort.Strings(leafKeys)

	for _, key := range leafKeys {
		value := lf.leaves[key]
		if recursive {
			value.Compact(recursive)
		}

		node, _ := materializeGetNode(value.Dir)
		lf.Dir.AddChild(context.Background(), key, node)

		//Do expensive link deletion cycle if a leaf was deleted (directory)
		if lf.leafDeleted == true {
			links, _ := lf.Dir.Links(context.Background())
			for _, v := range links {
				if v.Cid.Prefix().Codec == uint64(multicodec.Protobuf) {
					if lf.leaves[v.Name] == nil {
						//Deletion happened! Remove from directory structure
						lf.Dir.RemoveChild(context.Background(), v.Name)
					}
				}
			}
		}
	}
}

// Ipfs data tree directory wrapping with help functions
type DataBin struct {
	DataLayer *DataLayer
	Leaf      LeafDir
}

func (db *DataBin) Has(path string) bool {
	splitPath := strings.Split(path, "/")
	//Get directory regardless of child
	var qPath string
	if len(splitPath) > 1 {
		qPath = strings.Join(splitPath[:len(splitPath)-1], "/")
	} else {
		qPath = ""
	}
	wrkDir, err := db.resolveWrkDir(qPath)

	if err != nil {
		return false
	}
	endPath := splitPath[len(splitPath)-1]

	if wrkDir.leaves[endPath] != nil {
		return true
	}

	// Fast path: check the leaf entries cache populated during newLeafFromCid or Set.
	if wrkDir.leafEntries != nil {
		if _, ok := wrkDir.leafEntries[endPath]; ok {
			return true
		}
	}

	_, err = wrkDir.Dir.Find(context.Background(), endPath)
	return err == nil
}

// Later on support recursive directly look through
func (db *DataBin) List(prefix string) (*[]string, error) {

	wrkDir, err := db.resolveWrkDir(prefix)

	if err != nil {
		lsd := make([]string, 0)
		return &lsd, nil
	}

	links, err := wrkDir.Dir.Links(context.Background())

	names := make([]string, 0)
	for _, v := range links {
		names = append(names, v.Name)
	}

	if err != nil {
		return nil, err
	}

	tree := make([]string, 0)
	for _, v := range links {
		prefix := v.Cid.Prefix()
		if prefix.Codec == uint64(multicodec.Protobuf) {
			//If it's protobuf then it's a sub directory
			//TODO: Recursive resolve
			//Signify it's a directory by including "/" at the end
			tree = append(tree, v.Name+"/")
		} else {
			tree = append(tree, v.Name)
		}
	}

	sort.Strings(tree)
	return &tree, nil
}

func (db *DataBin) Set(path string, link cid.Cid) error {
	node, _ := db.DataLayer.DagServ.Get(context.Background(), link)

	var wrkDir uio.Directory
	splitPath := strings.Split(path, "/")

	var leaf *LeafDir
	if len(splitPath) > 1 {
		//Resolve working dir
		for idx, pathElement := range splitPath[:len(splitPath)-1] {
			if idx == 0 {
				leaf = &db.Leaf
			}
			nextLeaf := leaf.leaves[pathElement]
			if nextLeaf == nil {
				lf := &LeafDir{
					Dir:         uio.NewDirectory(db.DataLayer.DagServ),
					leaves:      make(map[string]*LeafDir),
					leafEntries: make(map[string]cid.Cid),
				}
				leaf.leaves[pathElement] = lf
				leaf = lf
			} else {
				leaf = nextLeaf
			}
		}
		wrkDir = leaf.Dir
	} else {
		leaf = &db.Leaf
		wrkDir = leaf.Dir
	}

	err := wrkDir.AddChild(context.Background(), splitPath[len(splitPath)-1], node)
	if err != nil {
		return err
	}
	if leaf.leafEntries == nil {
		leaf.leafEntries = make(map[string]cid.Cid)
	}
	leaf.leafEntries[splitPath[len(splitPath)-1]] = link
	return nil
}

func (db *DataBin) Get(path string) (*cid.Cid, error) {
	splitPath := strings.Split(path, "/")

	wrkDir, err := db.resolveWrkDir(strings.Join(splitPath[:len(splitPath)-1], "/"))
	if err != nil {
		return nil, os.ErrNotExist
	}

	endPath := splitPath[len(splitPath)-1]

	if wrkDir.leaves[endPath] != nil {
		//Do NOT allow directories to return CID
		//Breaks compaction logic and exposes mutable CID. Not valid K/V either.
		return nil, os.ErrNotExist
	}

	// Fast path: leaf entry CID was cached during newLeafFromCid or Set.
	if wrkDir.leafEntries != nil {
		if c, ok := wrkDir.leafEntries[endPath]; ok {
			return &c, nil
		}
	}

	// Slow path: fall back to HAMT Find (may hit blockstore for the data node).
	node, err := wrkDir.Dir.Find(context.Background(), endPath)
	if err != nil {
		return nil, err
	}
	c := node.Cid()
	// Populate the cache so subsequent reads for the same key are fast.
	if wrkDir.leafEntries == nil {
		wrkDir.leafEntries = make(map[string]cid.Cid)
	}
	wrkDir.leafEntries[endPath] = c
	return &c, nil
}

func (db *DataBin) Delete(path string) (bool, error) {
	splitPath := strings.Split(path, "/")
	//Get directory regardless of child
	var qPath string
	if len(splitPath) > 1 {
		qPath = strings.Join(splitPath[:len(splitPath)-1], "/")
	} else {
		qPath = ""
	}

	wrkDir, err := db.resolveWrkDir(qPath)

	if err != nil {
		return false, err
	}
	endPath := splitPath[len(splitPath)-1]

	if wrkDir.leaves[endPath] != nil {
		delete(wrkDir.leaves, endPath)
		wrkDir.Dir.RemoveChild(context.Background(), endPath)
		return true, nil
	} else {
		err := wrkDir.Dir.RemoveChild(context.Background(), endPath)
		if wrkDir.leafEntries != nil {
			delete(wrkDir.leafEntries, endPath)
		}
		if err == os.ErrNotExist {
			return false, nil
		} else {
			return true, nil
		}
	}
}

// Resolves working directory
// Errors out if path does not exist or path is a file
// Must be exact path to directory
func (db *DataBin) resolveWrkDir(path string) (*LeafDir, error) {
	splitPaths := strings.Split(path, "/")

	lf := &db.Leaf
	for _, path := range splitPaths {
		if path == "" {
			break
		}
		if lf.leaves[path] != nil {
			lf = lf.leaves[path]
		} else {
			return nil, errors.New("path does not exist")
		}
	}

	return lf, nil
}

// EnumerateAll returns all key-CID pairs in the DataBin.
// Keys for nested entries use "/" separator (e.g., "dir/subkey").
// This is used to rebuild state directories deterministically.
func (db *DataBin) EnumerateAll() (map[string]cid.Cid, error) {
	db.Leaf.Compact(true)
	result := make(map[string]cid.Cid)
	err := enumerateLeaf(&db.Leaf, "", result)
	return result, err
}

func enumerateLeaf(lf *LeafDir, prefix string, result map[string]cid.Cid) error {
	links, err := lf.Dir.Links(context.Background())
	if err != nil {
		return err
	}
	for _, link := range links {
		fullKey := link.Name
		if prefix != "" {
			fullKey = prefix + "/" + link.Name
		}
		if link.Cid.Prefix().Codec == uint64(multicodec.Protobuf) {
			// It's a subdirectory — recurse into its leaf if available
			if subLeaf, ok := lf.leaves[link.Name]; ok {
				if err := enumerateLeaf(subLeaf, fullKey, result); err != nil {
					return err
				}
			}
			// If no leaf, we can't enumerate further (data not loaded)
		} else {
			result[fullKey] = link.Cid
		}
	}
	return nil
}

// collectLeafCids traverses the LeafDir tree recursively and collects all unique
// leaf data CIDs stored in leafEntries maps. CIDs are deduplicated by string key.
func collectLeafCids(lf *LeafDir, seen map[string]bool, cids *[]cid.Cid) {
	for _, c := range lf.leafEntries {
		key := c.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		*cids = append(*cids, c)
	}
	for _, sub := range lf.leaves {
		collectLeafCids(sub, seen, cids)
	}
}

// prefetchLeaves eagerly fetches all leaf data blocks into the local blockstore
// so subsequent GetRaw calls hit Badger directly instead of falling through to
// bitswap. This is a best-effort optimization; failures are logged but not
// propagated since individual GetRaw calls will retry with their own timeout.
func (db *DataBin) prefetchLeaves() {
	seen := make(map[string]bool)
	var cids []cid.Cid
	collectLeafCids(&db.Leaf, seen, &cids)
	if len(cids) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.DataLayer.PrefetchBlocks(ctx, cids, 8); err != nil {
		log.Debug("databin: leaf prefetch completed with errors", "total", len(cids), "err", err)
	}
}

// Must compact to be safe. If single level
func (db *DataBin) Cid() cid.Cid {
	db.Leaf.Compact(true)
	node, _ := materializeGetNode(db.Leaf.Dir)

	return node.Cid()
}

func (db *DataBin) Save() cid.Cid {
	db.Leaf.Compact(true)
	nodeDir, err := materializeGetNode(db.Leaf.Dir)
	if err != nil {
		panic(err)
	}

	go func() {
		// links, _ := db.Leaf.Dir.Links(context.Background())
		var wg sync.WaitGroup
		// for _, link := range links {

		// 	wg.Add(1)
		// 	go func(link *format.Link) {
		// 		fmt.Println("Getting block", link.Cid)
		// 		blk, _ := db.DataLayer.blockServ.GetBlock(context.Background(), link.Cid)
		// 		fmt.Println("Notifying block", link.Cid)
		// 		db.DataLayer.notify(context.Background(), blk)
		// 		fmt.Println("Done block", link.Cid)
		// 		wg.Done()
		// 	}(link)
		// }
		wg.Wait()
		db.DataLayer.blockServ.AddBlock(context.Background(), nodeDir)
		db.DataLayer.bitswap.NotifyNewBlocks(context.Background(), nodeDir)

		db.DataLayer.p2pService.BroadcastCid(nodeDir.Cid())
	}()

	return nodeDir.Cid()
}

func NewDataBin(da *DataLayer) DataBin {
	uio.HAMTShardingSize = 256
	dir := uio.NewDirectory(da.DagServ)

	return DataBin{
		DataLayer: da,
		Leaf: LeafDir{
			Dir:         dir,
			leaves:      make(map[string]*LeafDir),
			leafEntries: make(map[string]cid.Cid),
		},
	}
}

func NewDataBinFromCid(da *DataLayer, inputCid cid.Cid) DataBin {
	uio.HAMTShardingSize = 256
	db := DataBin{
		DataLayer: da,
		Leaf:      newLeafFromCid(da, inputCid),
	}
	db.prefetchLeaves()
	return db
}

func newLeafFromCid(da *DataLayer, inputCid cid.Cid) LeafDir {
	ctx := context.Background()

	node, err := da.DagServ.Get(ctx, inputCid)
	if err != nil {
		log.Warn("databin: error loading node for CID", "cid", inputCid, "err", err)
		// Return empty leaf to avoid panic
		return LeafDir{
			Dir:    uio.NewDirectory(da.DagServ),
			leaves: make(map[string]*LeafDir),
		}
	}

	dir, err := uio.NewDirectoryFromNode(da.DagServ, node)
	if err != nil {
		log.Warn("databin: error creating directory from node", "cid", inputCid, "err", err)
		return LeafDir{
			Dir:    uio.NewDirectory(da.DagServ),
			leaves: make(map[string]*LeafDir),
		}
	}

	links, _ := dir.Links(ctx)

	leaves := make(map[string]*LeafDir)
	leafEntries := make(map[string]cid.Cid)
	for _, lnk := range links {
		if lnk.Cid.Prefix().Codec == uint64(multicodec.Protobuf) {
			lf := newLeafFromCid(da, lnk.Cid)
			leaves[lnk.Name] = &lf
		} else {
			// Cache leaf entry CID so Get/Has can avoid the Dir.Find blockstore read.
			leafEntries[lnk.Name] = lnk.Cid
		}
	}

	return LeafDir{
		Dir:         dir,
		leaves:      leaves,
		leafEntries: leafEntries,
	}
}
