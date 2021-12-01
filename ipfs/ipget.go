package ipfs

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"os/signal"
	gopath "path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/cheggaaa/pb"
	config "github.com/ipfs/go-ipfs-config"
	files "github.com/ipfs/go-ipfs-files"
	ipfshttp "github.com/ipfs/go-ipfs-http-client"
	"github.com/ipfs/go-ipfs/core"
	"github.com/ipfs/go-ipfs/core/coreapi"
	"github.com/ipfs/go-ipfs/core/node/libp2p"
	"github.com/ipfs/go-ipfs/plugin/loader"
	"github.com/ipfs/go-ipfs/repo/fsrepo"
	iface "github.com/ipfs/interface-go-ipfs-core"
	"github.com/ipfs/interface-go-ipfs-core/options"
	ipath "github.com/ipfs/interface-go-ipfs-core/path"
	peer "github.com/libp2p/go-libp2p-core/peer"
	ma "github.com/multiformats/go-multiaddr"
	cli "github.com/urfave/cli/v2"
)

var (
	cleanup      []func() error
	cleanupMutex sync.Mutex
)

func Get(input, output string) {

	opath := output
	ipath := input
	// namm := "env"
	fmt.Println("opath:=", opath)
	fmt.Println("ipath:=", ipath)
	// fmt.Println("namm:=", namm)

	// Do any cleanup on exit
	defer doCleanup()
	//

	//====全不要
	app := cli.NewApp()
	app.Name = "ipget"
	app.Usage = "Retrieve and save IPFS objects."
	app.Version = "0.7.0"
	app.Flags = []cli.Flag{
		&cli.StringFlag{
			Name:    "output",
			Aliases: []string{"o"},
			Usage:   "specify output location",
		},
		&cli.StringFlag{
			Name:    "node",
			Aliases: []string{"n"},
			Usage:   "specify ipfs node strategy (\"local\", \"spawn\", \"temp\" or \"fallback\")",
			Value:   "fallback",
		},
		&cli.StringSliceFlag{
			Name:    "peers",
			Aliases: []string{"p"},
			Usage:   "specify a set of IPFS peers to connect to",
		},
		&cli.BoolFlag{
			Name:  "progress",
			Usage: "show a progress bar",
		},
	}
	//==========End

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigExitCoder := make(chan cli.ExitCoder, 1)

	app.Action = func(c *cli.Context) error {
		outPath := c.String("output")
		if opath != "" {
			outPath = opath
		}

		iPath, err := parsePath(ipath)
		if err != nil {
			return err
		}

		// Use the final segment of the object's path if no path was given.
		if outPath == "" {
			trimmed := strings.TrimRight(iPath.String(), "/")
			_, outPath = filepath.Split(trimmed)
			outPath = filepath.Clean(outPath)
		}

		var ipfs iface.CoreAPI
		switch c.String("node") {
		case "fallback":
			ipfs, err = http(ctx)
			if err == nil {
				break
			}
			fallthrough
		case "spawn":
			ipfs, err = spawn(ctx)
		case "local":
			ipfs, err = http(ctx)
		case "temp":
			ipfs, err = temp(ctx)
		default:
			return fmt.Errorf("no such 'node' strategy, %q", c.String("node"))
		}
		if err != nil {
			return err
		}

		go connect(ctx, ipfs, c.StringSlice("peers"))

		out, err := ipfs.Unixfs().Get(ctx, iPath)
		if err != nil {
			if err == context.Canceled {
				return <-sigExitCoder
			}
			return cli.Exit(err, 2)
		}
		err = WriteTo(out, outPath, c.Bool("progress"))
		if err != nil {
			if err == context.Canceled {
				return <-sigExitCoder
			}
			return cli.Exit(err, 2)
		}
		return nil
	}
	// Catch interrupt signal
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigs
		sigExitCoder <- cli.Exit("", 128+int(sig.(syscall.Signal)))
		cancel()
	}()

	// cli library requires flags before arguments
	args := movePostfixOptions(os.Args)

	err := app.Run(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		doCleanup()
		os.Exit(1)
	}
}

// movePostfixOptions moves non-flag arguments to end of argument list.
func movePostfixOptions(args []string) []string {
	var endArgs []string
	for idx := 1; idx < len(args); idx++ {
		if args[idx][0] == '-' {
			if !strings.Contains(args[idx], "=") {
				idx++
			}
			continue
		}
		if endArgs == nil {
			// on first write, make copy of args
			newArgs := make([]string, len(args))
			copy(newArgs, args)
			args = newArgs
		}
		// add to args accumulator
		endArgs = append(endArgs, args[idx])
		// remove from real args list
		args = args[:idx+copy(args[idx:], args[idx+1:])]
		idx--
	}

	// append extracted arguments to the real args
	return append(args, endArgs...)
}

func parsePath(path string) (ipath.Path, error) {
	ipfsPath := ipath.New(path)
	if ipfsPath.IsValid() == nil {
		return ipfsPath, nil
	}

	u, err := url.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("%q could not be parsed: %s", path, err)
	}

	switch proto := u.Scheme; proto {
	case "ipfs", "ipld", "ipns":
		ipfsPath = ipath.New(gopath.Join("/", proto, u.Host, u.Path))
	case "http", "https":
		ipfsPath = ipath.New(u.Path)
	default:
		return nil, fmt.Errorf("%q is not recognized as an IPFS path", path)
	}
	return ipfsPath, ipfsPath.IsValid()
}

// WriteTo writes the given node to the local filesystem at fpath.
func WriteTo(nd files.Node, fpath string, progress bool) error {
	s, err := nd.Size()
	if err != nil {
		return err
	}

	var bar *pb.ProgressBar
	if progress {
		bar = pb.New64(s).Start()
	}

	return writeToRec(nd, fpath, bar)
}

func writeToRec(nd files.Node, fpath string, bar *pb.ProgressBar) error {
	switch nd := nd.(type) {
	case *files.Symlink:
		return os.Symlink(nd.Target, fpath)
	case files.File:
		f, err := os.Create(fpath)
		defer f.Close()
		if err != nil {
			return err
		}

		var r io.Reader = nd
		if bar != nil {
			r = bar.NewProxyReader(r)
		}
		_, err = io.Copy(f, r)
		if err != nil {
			return err
		}
		return nil
	case files.Directory:
		err := os.Mkdir(fpath, 0777)
		if err != nil {
			return err
		}

		entries := nd.Entries()
		for entries.Next() {
			child := filepath.Join(fpath, entries.Name())
			if err := writeToRec(entries.Node(), child, bar); err != nil {
				return err
			}
		}
		return entries.Err()
	default:
		return fmt.Errorf("file type %T at %q is not supported", nd, fpath)
	}
}

func addCleanup(f func() error) {
	cleanupMutex.Lock()
	defer cleanupMutex.Unlock()
	cleanup = append(cleanup, f)
}

func doCleanup() {
	cleanupMutex.Lock()
	defer cleanupMutex.Unlock()

	for _, f := range cleanup {
		if err := f(); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}
}

func connect(ctx context.Context, ipfs iface.CoreAPI, peers []string) error {
	var wg sync.WaitGroup
	pinfos := make(map[peer.ID]*peer.AddrInfo, len(peers))
	for _, addrStr := range peers {
		addr, err := ma.NewMultiaddr(addrStr)
		if err != nil {
			return err
		}
		pii, err := peer.AddrInfoFromP2pAddr(addr)
		if err != nil {
			return err
		}
		pi, ok := pinfos[pii.ID]
		if !ok {
			pi = &peer.AddrInfo{ID: pii.ID}
			pinfos[pi.ID] = pi
		}
		pi.Addrs = append(pi.Addrs, pii.Addrs...)
	}

	wg.Add(len(pinfos))
	for _, pi := range pinfos {
		go func(pi *peer.AddrInfo) {
			defer wg.Done()
			log.Printf("attempting to connect to peer: %q\n", pi)
			err := ipfs.Swarm().Connect(ctx, *pi)
			if err != nil {
				log.Printf("failed to connect to %s: %s", pi.ID, err)
			}
			log.Printf("successfully connected to %s\n", pi.ID)
		}(pi)
	}
	wg.Wait()
	return nil
}
func http(ctx context.Context) (iface.CoreAPI, error) {
	httpApi, err := ipfshttp.NewLocalApi()
	if err != nil {
		return nil, err
	}
	err = httpApi.Request("version").Exec(ctx, nil)
	if err != nil {
		return nil, err
	}
	return httpApi, nil
}

type CfgOpt func(*config.Config)

func spawn(ctx context.Context) (iface.CoreAPI, error) {
	defaultPath, err := config.PathRoot()
	if err != nil {
		// shouldn't be possible
		return nil, err
	}

	if err := setupPlugins(defaultPath); err != nil {
		return nil, err
	}

	ipfs, err := open(ctx, defaultPath)
	if err == nil {
		return ipfs, nil
	}

	return tmpNode(ctx)
}

func setupPlugins(path string) error {
	// Load plugins. This will skip the repo if not available.
	plugins, err := loader.NewPluginLoader(filepath.Join(path, "plugins"))
	if err != nil {
		return fmt.Errorf("error loading plugins: %s", err)
	}

	if err := plugins.Initialize(); err != nil {
		return fmt.Errorf("error initializing plugins: %s", err)
	}

	if err := plugins.Inject(); err != nil {
		return fmt.Errorf("error initializing plugins: %s", err)
	}

	return nil
}

func open(ctx context.Context, repoPath string) (iface.CoreAPI, error) {
	// Open the repo
	r, err := fsrepo.Open(repoPath)
	if err != nil {
		return nil, err
	}

	// Construct the node
	node, err := core.NewNode(ctx, &core.BuildCfg{
		Online:  true,
		Routing: libp2p.DHTClientOption,
		Repo:    r,
	})
	if err != nil {
		return nil, err
	}

	return coreapi.NewCoreAPI(node)
}

func temp(ctx context.Context) (iface.CoreAPI, error) {
	defaultPath, err := config.PathRoot()
	if err != nil {
		// shouldn't be possible
		return nil, err
	}

	if err := setupPlugins(defaultPath); err != nil {
		return nil, err
	}

	return tmpNode(ctx)
}

func tmpNode(ctx context.Context) (iface.CoreAPI, error) {
	dir, err := ioutil.TempDir("", "ipfs-shell")
	if err != nil {
		return nil, fmt.Errorf("failed to get temp dir: %s", err)
	}

	// Cleanup temp dir on exit
	addCleanup(func() error {
		return os.RemoveAll(dir)
	})

	identity, err := config.CreateIdentity(ioutil.Discard, []options.KeyGenerateOption{
		options.Key.Type(options.Ed25519Key),
	})
	if err != nil {
		return nil, err
	}
	cfg, err := config.InitWithIdentity(identity)
	if err != nil {
		return nil, err
	}

	// configure the temporary node
	cfg.Routing.Type = "dhtclient"

	err = fsrepo.Init(dir, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to init ephemeral node: %s", err)
	}
	return open(ctx, dir)
}
