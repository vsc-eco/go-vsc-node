package datalayer

import (
	"context"
	"errors"
	"os"
	"sort"
	"strings"
	"sync"

	uio "github.com/ipfs/boxo/ipld/unixfs/io"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multicodec"
)

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
	for key, value := range lf.leaves {
		if recursive {
			value.Compact(recursive)
		}

		node, _ := value.Dir.GetNode()
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

	wrkDir, _ := db.resolveWrkDir(strings.Join(splitPath[:len(splitPath)-1], "/"))

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

// Must compact to be safe. If single level
func (db *DataBin) Cid() cid.Cid {
	db.Leaf.Compact(true)
	node, _ := db.Leaf.Dir.GetNode()

	return node.Cid()
}

func (db *DataBin) Save() cid.Cid {
	db.Leaf.Compact(true)
	nodeDir, err := db.Leaf.Dir.GetNode()
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
	uio.HAMTShardingSize = 1
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
	return DataBin{
		DataLayer: da,
		Leaf:      newLeafFromCid(da, inputCid),
	}
}

func newLeafFromCid(da *DataLayer, inputCid cid.Cid) LeafDir {
	ctx := context.Background()

	node, _ := da.DagServ.Get(ctx, inputCid)
	dir, _ := uio.NewDirectoryFromNode(da.DagServ, node)

	links, _ := dir.Links(ctx)

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
	}
}
