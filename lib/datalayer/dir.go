package datalayer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	mdag "github.com/ipfs/boxo/ipld/merkledag"
	ft "github.com/ipfs/boxo/ipld/unixfs"
	"github.com/ipfs/boxo/ipld/unixfs/hamt"
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

	// fmt.Println("End pathing for search", endPath)

	if wrkDir.leaves[endPath] != nil {
		return true
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
					Dir:    uio.NewDirectory(db.DataLayer.DagServ),
					leaves: make(map[string]*LeafDir),
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

	// dag, _ := dagCbor.Decode(nodeDir.RawData(), mh.SHA2_256, -1)
	// json := dag.RawData()

	// fmt.Println("Json", json)
	return err
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

	node, err := wrkDir.Dir.Find(context.Background(), endPath)

	// listo, _ := wrkDir.Dir.Links(context.Background())

	if err != nil {

		return nil, err
	} else {
		cid := node.Cid()
		return &cid, nil
	}
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
		if db.DataLayer.bitswap == nil {
			// The LocalOnly view has no network side: the root stays local.
			return
		}
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
			Dir:    dir,
			leaves: make(map[string]*LeafDir),
		},
	}
}

func NewDataBinFromCid(da *DataLayer, inputCid cid.Cid) DataBin {
	uio.HAMTShardingSize = 256
	return DataBin{
		DataLayer: da,
		Leaf:      newLeafFromCid(da, inputCid),
	}
}

// newLeafFromCid loads the state directory at inputCid. Contract execution and
// the block producer read state through here, so a failed read must not turn
// into an empty directory: a node whose blockstore hiccups would then run a
// contract against empty state while its peers read the real one, and its
// result would silently differ. On the full data layer a failed read is retried
// until it succeeds, so the node either computes the same result as its peers or
// makes no progress, the same fail-stop rule as the ledger reads. The LocalOnly
// view keeps answering with an empty directory: API reads (where a CID the node
// does not hold is expected) and the oracle relay and TSS gates, which run on
// goroutines or under locks that must not block.
func newLeafFromCid(da *DataLayer, inputCid cid.Cid) LeafDir {
	const (
		baseDelay = 100 * time.Millisecond
		maxDelay  = 30 * time.Second
	)
	delay := baseDelay
	for attempt := 1; ; attempt++ {
		leaf, err := loadLeafFromCid(da, inputCid)
		if err == nil {
			if attempt > 1 {
				log.Error("databin: state read recovered; resuming", "cid", inputCid, "attempts", attempt)
			}
			return leaf
		}
		if da.localOnly {
			log.Warn("databin: state not available locally", "cid", inputCid, "err", err)
			return LeafDir{
				Dir:    uio.NewDirectory(da.DagServ),
				leaves: make(map[string]*LeafDir),
			}
		}
		log.Error("databin: state read failed; blocking until it succeeds", "cid", inputCid, "attempt", attempt, "err", err)
		time.Sleep(delay)
		if delay *= 2; delay > maxDelay {
			delay = maxDelay
		}
	}
}

func loadLeafFromCid(da *DataLayer, inputCid cid.Cid) (LeafDir, error) {
	ctx := context.Background()

	node, err := da.DagServ.Get(ctx, inputCid)
	if err != nil {
		return LeafDir{}, fmt.Errorf("loading node: %w", err)
	}

	dir, err := uio.NewDirectoryFromNode(da.DagServ, node)
	if err != nil {
		return LeafDir{}, fmt.Errorf("directory from node: %w", err)
	}

	links, err := dir.Links(ctx)
	if err != nil {
		return LeafDir{}, fmt.Errorf("listing links: %w", err)
	}

	leaves := make(map[string]*LeafDir)
	for _, lnk := range links {
		if lnk.Cid.Prefix().Codec == uint64(multicodec.Protobuf) {
			lf := newLeafFromCid(da, lnk.Cid)
			leaves[lnk.Name] = &lf
		}
	}

	return LeafDir{
		Dir:    dir,
		leaves: leaves,
	}, nil
}

// GetPathCid resolves one key of the state directory rooted at root and returns
// exactly what NewDataBinFromCid(da, root).Get(path) returns, but loads only the
// nodes on the key's path (the root, the HAMT shards down to the entry, the
// parent directories, the value) instead of every entry of the whole tree,
// which loadLeafFromCid enumerates recursively. 18u (testnet 11-02): the BTC
// theft-halt mirror read the whole BTC contract state every block to look up
// one key; a node catching up processed blocks at a crawl (pprof:
// loadLeafFromCid).
//
// Same classification as the DataBin: a link whose codec is multicodec.Protobuf
// is a directory (loadLeafFromCid), so an intermediate segment must be one and
// the last segment must not be (Get refuses a directory); an empty segment ends
// the walk (resolveWrkDir). Same failure rules: a missing name is
// os.ErrNotExist; a failure to read the directory path (root, HAMT shards,
// parent directories) follows newLeafFromCid: retried until it succeeds on the
// full data layer (fail-stop), an absent key on the LocalOnly view (its empty
// directory); a failure to read the value itself is returned as it is, as
// Directory.Find returns the value fetch's error (an unreadable value fails
// closed, it does not read as absent). One difference, in a degenerate state
// only: the DataBin loses every key when any shard of the state is missing
// locally, GetPathCid only the keys whose own path is missing.
func GetPathCid(da *DataLayer, root cid.Cid, path string) (*cid.Cid, error) {
	const (
		baseDelay = 100 * time.Millisecond
		maxDelay  = 30 * time.Second
	)
	delay := baseDelay
	for attempt := 1; ; attempt++ {
		c, err, dirErr := getPathCidOnce(da, root, path)
		if err == nil || errors.Is(err, os.ErrNotExist) || !dirErr {
			if attempt > 1 {
				log.Error("databin: state key read recovered; resuming", "cid", root, "attempts", attempt)
			}
			return c, err
		}
		if da.localOnly {
			return nil, os.ErrNotExist
		}
		log.Error("databin: state key read failed; blocking until it succeeds", "cid", root, "key", path, "attempt", attempt, "err", err)
		time.Sleep(delay)
		if delay *= 2; delay > maxDelay {
			delay = maxDelay
		}
	}
}

// getPathCidOnce is one attempt. dirErr reports a failure to read the
// directory path (retried / absent), as opposed to the value itself.
func getPathCidOnce(da *DataLayer, root cid.Cid, path string) (_ *cid.Cid, err error, dirErr bool) {
	ctx := context.Background()
	dirNode, err := da.DagServ.Get(ctx, root)
	if err != nil {
		return nil, fmt.Errorf("loading node: %w", err), true
	}
	segs := strings.Split(path, "/")
	for _, seg := range segs[:len(segs)-1] {
		if seg == "" {
			break
		}
		lnk, err := findLink(ctx, da.DagServ, dirNode, seg)
		if errors.Is(err, os.ErrNotExist) {
			return nil, os.ErrNotExist, false
		}
		if err != nil {
			return nil, err, true
		}
		if lnk.Cid.Prefix().Codec != uint64(multicodec.Protobuf) {
			return nil, os.ErrNotExist, false // not a directory in the DataBin's terms
		}
		if dirNode, err = da.DagServ.Get(ctx, lnk.Cid); err != nil {
			return nil, fmt.Errorf("loading node: %w", err), true
		}
	}
	lnk, err := findLink(ctx, da.DagServ, dirNode, segs[len(segs)-1])
	if errors.Is(err, os.ErrNotExist) {
		return nil, os.ErrNotExist, false
	}
	if err != nil {
		return nil, err, true
	}
	if lnk.Cid.Prefix().Codec == uint64(multicodec.Protobuf) {
		return nil, os.ErrNotExist, false // a directory is not a value (Get refuses it)
	}
	if _, err := da.DagServ.Get(ctx, lnk.Cid); err != nil {
		return nil, err, false // the value fetch, as Directory.Find returns it
	}
	c := lnk.Cid
	return &c, nil, false
}

// findLink is uio Directory.Find without fetching the entry: the link of name
// in the directory node dirNode, typed as uio.NewDirectoryFromNode types it
// (BasicDirectory: GetNodeLink; HAMT shard: Shard.Find). A missing name is
// os.ErrNotExist.
func findLink(ctx context.Context, dserv ipld.DAGService, dirNode ipld.Node, name string) (*ipld.Link, error) {
	pn, ok := dirNode.(*mdag.ProtoNode)
	if !ok {
		return nil, uio.ErrNotADir
	}
	fsn, err := ft.FSNodeFromBytes(pn.Data())
	if err != nil {
		return nil, err
	}
	switch fsn.Type() {
	case ft.TDirectory:
		lnk, err := pn.GetNodeLink(name)
		if errors.Is(err, mdag.ErrLinkNotFound) {
			return nil, os.ErrNotExist
		}
		return lnk, err
	case ft.THAMTShard:
		shard, err := hamt.NewHamtFromDag(dserv, dirNode)
		if err != nil {
			return nil, err
		}
		return shard.Find(ctx, name)
	default:
		return nil, uio.ErrNotADir
	}
}
