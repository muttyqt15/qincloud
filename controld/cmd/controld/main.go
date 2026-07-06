// main.go — controld CLI, the QinCloud control plane. Runs in a container
// with docker.sock + the caddy admin socket + data_net. Subcommands:
//
//	serve                                   long-running process (healthz; M5 dashboard mounts here)
//	deploy -app X -image Y -port N -host H  run one deploy to live
//	list                                    apps and their live containers
//	destroy -app X                          remove route, containers, record
//
// Drive it via: docker exec qincloud-controld controld <cmd> ...
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"text/tabwriter"
	"time"

	"qincloud/controld/internal/caddyapi"
	"qincloud/controld/internal/deploy"
	"qincloud/controld/internal/dockerx"
	"qincloud/controld/internal/store"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve()
	case "deploy":
		err = deployCmd(os.Args[2:])
	case "list":
		err = listCmd()
	case "destroy":
		err = destroyCmd(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		log.Fatalf("controld %s: %v", os.Args[1], err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: controld <serve | deploy | list | destroy>
  serve
  deploy  -app <name> -image <ref> -port <containerPort> -host <hostname>
  list
  destroy -app <name>`)
	os.Exit(2)
}

// serve is the container's main process: liveness now, dashboard in M5.
func serve() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	srv := &http.Server{
		Addr:    ":8600",
		Handler: mux,
		// Never trust a client to finish its headers: without this a stalled
		// connection holds its goroutine forever (slowloris).
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Println("controld serving on :8600")
	return srv.ListenAndServe()
}

// wire builds the deployer from its four capabilities. Linear, no factories:
// this is the entire dependency graph of the control plane.
func wire(ctx context.Context) (*deploy.Deployer, func(), error) {
	dsn := mustEnv("CONTROLD_DSN")
	st, err := store.Connect(ctx, dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("store: %w", err)
	}
	if err := st.Init(ctx); err != nil {
		st.Close()
		return nil, nil, fmt.Errorf("apply schema: %w", err)
	}
	dk, err := dockerx.New()
	if err != nil {
		st.Close()
		return nil, nil, fmt.Errorf("docker: %w", err)
	}
	rt := caddyapi.New(envOr("CADDY_ADMIN_SOCK", "/run/caddy/admin.sock"))
	// Per-app cross-process lock, on the same database the store uses.
	lk := &pgAppLock{dsn: dsn}
	cleanup := func() { st.Close(); _ = dk.Close() }
	return deploy.New(dk, rt, st, lk), cleanup, nil
}

func deployCmd(args []string) error {
	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	var spec deploy.AppSpec
	fs.StringVar(&spec.Name, "app", "", "app name ([a-z0-9-])")
	fs.StringVar(&spec.Image, "image", "", "image ref")
	fs.IntVar(&spec.ContainerPort, "port", 0, "port the app listens on in the container")
	fs.StringVar(&spec.Host, "host", "", "hostname to route to this app")
	_ = fs.Parse(args) // ExitOnError: Parse never returns an error

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	d, cleanup, err := wire(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := d.Deploy(ctx, spec); err != nil {
		return err
	}
	log.Printf("%s is live: http://%s → %s", spec.Name, spec.Host, spec.Image)
	return nil
}

func listCmd() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := store.Connect(ctx, mustEnv("CONTROLD_DSN"))
	if err != nil {
		return err
	}
	defer st.Close()
	// Init here too, not just in wire(): `list` is the natural first command
	// on a rebuilt box, and a raw `relation "apps" does not exist` mid-DR is
	// the wrong moment for a confusing error. Init is idempotent.
	if err := st.Init(ctx); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	apps, err := st.ListApps(ctx)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "APP\tHOST\tIMAGE\tCONTAINER\tUPDATED")
	for _, a := range apps {
		cid := a.ContainerID
		if cid == "" {
			cid = "-"
		} else if len(cid) > 12 {
			cid = cid[:12]
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			a.Name, a.Host, a.Image, cid, a.UpdatedAt.Format(time.RFC3339))
	}
	return w.Flush()
}

func destroyCmd(args []string) error {
	fs := flag.NewFlagSet("destroy", flag.ExitOnError)
	app := fs.String("app", "", "app name")
	_ = fs.Parse(args)
	if *app == "" {
		return fmt.Errorf("-app is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	d, cleanup, err := wire(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := d.Destroy(ctx, *app); err != nil {
		return err
	}
	log.Printf("%s destroyed", *app)
	return nil
}

// mustEnv fails loud on missing required config — a control plane with a
// silently-defaulted DSN is broken, not degraded.
func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required env %s is not set", key)
	}
	return v
}

// envOr is for genuinely optional config with a fixed convention (the admin
// socket path is set by the compose mounts, overridable for tests).
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
