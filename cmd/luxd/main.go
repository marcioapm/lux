// Command luxd is the lux control plane.
//
//	luxd migrate                                  apply migrations (as the database owner)
//	luxd admin create-tenant --name N             → {"tenantId", "apiKey"}
//	luxd admin create-key --tenant T [--scopes run,read]
//	luxd admin create-host-token [--tenant T] [--pool P] [--label k=v]  → {"token"}
//	luxd admin create-pool --name N --provider static|ec2 [--tenant T] [--shared] ...
//	luxd admin set-quota --tenant T [--max-runs N] [--max-hosts N] [--retention-days N]
//	luxd serve                                    run the API, scheduler and reapers
//
// Configuration is environment variables; see docs/operations.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/blob"
	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/server"
	"github.com/marcioapm/lux/internal/store"
	"github.com/marcioapm/lux/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "migrate":
		err = migrate(ctx)
	case "admin":
		err = admin(ctx, os.Args[2:])
	case "serve":
		err = serve(ctx)
	case "version", "--version":
		fmt.Println(version.Version)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "luxd:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: luxd migrate | admin <command> | serve | version

admin commands:
  create-tenant --name N [--max-runs N] [--max-hosts N] [--retention-days N]
  create-key --tenant T [--name N] [--scopes read,run,admin]
  create-host-token [--tenant T] [--pool P] [--label k=v ...]
  create-pool --name N --provider static|ec2 [--tenant T] [--shared]
              [--min N] [--max N] [--warm N] [--template JSON]
  set-quota --tenant T [--max-runs N] [--max-hosts N] [--max-storage BYTES] [--retention-days N]`)
	os.Exit(2)
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func mustEnv(name string) (string, error) {
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return v, nil
}

func migrate(ctx context.Context) error {
	dsn, err := mustEnv("LUX_DATABASE_URL")
	if err != nil {
		return err
	}
	applied, err := store.Migrate(ctx, dsn, env("LUX_APP_PASSWORD", "lux_app"))
	for _, v := range applied {
		fmt.Fprintln(os.Stderr, "applied", v)
	}
	return err
}

func serve(ctx context.Context) error {
	level := slog.LevelInfo
	if os.Getenv("LUX_DEBUG") != "" {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	dsn, err := mustEnv("LUX_DATABASE_URL")
	if err != nil {
		return err
	}
	db, err := store.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	bucket, err := mustEnv("LUX_S3_BUCKET")
	if err != nil {
		return err
	}
	blobs, err := blob.New(blob.Config{
		Endpoint:       os.Getenv("LUX_S3_ENDPOINT"),
		PublicEndpoint: os.Getenv("LUX_S3_PUBLIC_ENDPOINT"),
		Region:         env("LUX_S3_REGION", "us-east-1"),
		Bucket:         bucket,
		AccessKey:      os.Getenv("LUX_S3_ACCESS_KEY"),
		SecretKey:      os.Getenv("LUX_S3_SECRET_KEY"),
	})
	if err != nil {
		return err
	}
	if err := blobs.Check(ctx); err != nil {
		return fmt.Errorf("blob store: %w", err)
	}
	cfg := server.Config{
		Listen:    env("LUX_LISTEN", "127.0.0.1:7070"),
		PublicURL: os.Getenv("LUX_PUBLIC_URL"),
	}
	if cfg.LeaseDuration, err = durationEnv("LUX_LEASE", 30*time.Second); err != nil {
		return err
	}
	if cfg.Tick, err = durationEnv("LUX_TICK", time.Second); err != nil {
		return err
	}
	if cfg.ScaleDownAfter, err = durationEnv("LUX_SCALE_DOWN_AFTER", 10*time.Minute); err != nil {
		return err
	}
	if cfg.LaunchTimeout, err = durationEnv("LUX_LAUNCH_TIMEOUT", 10*time.Minute); err != nil {
		return err
	}
	cfg.Providers, err = providers(ctx, log)
	if err != nil {
		return err
	}
	srv := server.New(cfg, db, blobs, log)
	return srv.Run(ctx)
}

func durationEnv(name string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return d, nil
}

type labelsFlag map[string]string

func (l labelsFlag) String() string { return "" }
func (l labelsFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return errors.New("want key=value")
	}
	l[k] = val
	return nil
}

func admin(ctx context.Context, args []string) error {
	if len(args) == 0 {
		usage()
	}
	dsn, err := mustEnv("LUX_DATABASE_URL")
	if err != nil {
		return err
	}
	db, err := store.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	out := json.NewEncoder(os.Stdout)
	fs := flag.NewFlagSet(args[0], flag.ExitOnError)
	sys := store.System()

	switch args[0] {
	case "create-tenant":
		name := fs.String("name", "", "tenant name")
		maxRuns := fs.Int("max-runs", 0, "max concurrent runs (0: unlimited)")
		maxHosts := fs.Int("max-hosts", 0, "max hosts (0: unlimited)")
		retention := fs.Int("retention-days", 30, "days to keep finished runs' blobs")
		fs.Parse(args[1:])
		if *name == "" {
			return errors.New("--name is required")
		}
		tenantID := ids.New(ids.Tenant)
		key := ids.Secret("lux")
		err := db.Tx(ctx, sys, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, name, retention_days, max_concurrent_runs, max_hosts)
				VALUES ($1, $2, $3, nullif($4, 0), nullif($5, 0))`, tenantID, *name, *retention, *maxRuns, *maxHosts); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ($1, $2, 'bootstrap', $3, $4)`,
				ids.New(ids.APIKey), tenantID, ids.Hash(key), []string{"admin"})
			return err
		})
		if err != nil {
			return err
		}
		return out.Encode(map[string]string{"tenantId": tenantID, "apiKey": key})

	case "create-key":
		tenant := fs.String("tenant", "", "tenant id")
		name := fs.String("name", "key", "key name")
		scopes := fs.String("scopes", "run", "comma-separated: read, run, admin")
		fs.Parse(args[1:])
		if *tenant == "" {
			return errors.New("--tenant is required")
		}
		sc := strings.Split(*scopes, ",")
		for _, s := range sc {
			if s != "read" && s != "run" && s != "admin" {
				return fmt.Errorf("unknown scope %q", s)
			}
		}
		key := ids.Secret("lux")
		err := db.Tx(ctx, sys, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ($1, $2, $3, $4, $5)`,
				ids.New(ids.APIKey), *tenant, *name, ids.Hash(key), sc)
			return err
		})
		if err != nil {
			return err
		}
		return out.Encode(map[string]string{"apiKey": key})

	case "create-host-token":
		tenant := fs.String("tenant", "", "tenant id (empty: a platform host)")
		pool := fs.String("pool", "default", "pool the hosts join")
		labels := labelsFlag{}
		fs.Var(labels, "label", "host label key=value (repeatable)")
		fs.Parse(args[1:])
		token := ids.Secret("luxh")
		err := db.Tx(ctx, sys, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO host_tokens (id, tenant_id, pool, labels, token_hash) VALUES ($1, nullif($2, ''), $3, $4, $5)`,
				ids.New(ids.HostToken), *tenant, *pool, map[string]string(labels), ids.Hash(token))
			return err
		})
		if err != nil {
			return err
		}
		return out.Encode(map[string]string{"token": token})

	case "create-pool":
		name := fs.String("name", "", "pool name")
		provider := fs.String("provider", "static", "static | ec2")
		tenant := fs.String("tenant", "", "tenant id (empty: a platform pool)")
		shared := fs.Bool("shared", false, "platform pool whose hosts may run several tenants' Runs")
		minH := fs.Int("min", 0, "minimum hosts")
		maxH := fs.Int("max", 0, "maximum hosts")
		warm := fs.Int("warm", 0, "idle hosts to keep ready")
		template := fs.String("template", "{}", "provider template (JSON)")
		fs.Parse(args[1:])
		var tmpl map[string]any
		if err := json.Unmarshal([]byte(*template), &tmpl); err != nil {
			return fmt.Errorf("--template: %w", err)
		}
		if *shared && *tenant != "" {
			return errors.New("only platform pools (no --tenant) can be shared")
		}
		id := ids.New(ids.Pool)
		err := db.Tx(ctx, sys, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO pools (id, tenant_id, name, provider, template, min_hosts, max_hosts, warm_hosts, shared)
				VALUES ($1, nullif($2, ''), $3, $4, $5, $6, $7, $8, $9)
				ON CONFLICT (coalesce(tenant_id, ''), name) DO UPDATE SET provider = EXCLUDED.provider, template = EXCLUDED.template,
					min_hosts = EXCLUDED.min_hosts, max_hosts = EXCLUDED.max_hosts, warm_hosts = EXCLUDED.warm_hosts, shared = EXCLUDED.shared,
					retired = false`,
				id, *tenant, *name, *provider, tmpl, *minH, *maxH, *warm, *shared)
			return err
		})
		if err != nil {
			return err
		}
		return out.Encode(map[string]string{"pool": *name})

	case "set-quota":
		tenant := fs.String("tenant", "", "tenant id")
		maxRuns := fs.Int("max-runs", -1, "max concurrent runs (0: unlimited)")
		maxHosts := fs.Int("max-hosts", -1, "max hosts (0: unlimited)")
		maxStorage := fs.Int64("max-storage", -1, "max stored bytes (0: unlimited)")
		retention := fs.Int("retention-days", -1, "days to keep finished runs' blobs")
		fs.Parse(args[1:])
		err := db.Tx(ctx, sys, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE tenants SET
					max_concurrent_runs = CASE WHEN $2 < 0 THEN max_concurrent_runs ELSE nullif($2, 0) END,
					max_hosts = CASE WHEN $3 < 0 THEN max_hosts ELSE nullif($3, 0) END,
					max_storage_bytes = CASE WHEN $4 < 0 THEN max_storage_bytes ELSE nullif($4, 0) END,
					retention_days = CASE WHEN $5 < 0 THEN retention_days ELSE $5 END
				WHERE id = $1`, *tenant, *maxRuns, *maxHosts, *maxStorage, *retention)
			return err
		})
		if err != nil {
			return err
		}
		return out.Encode(map[string]bool{"ok": true})
	}
	usage()
	return nil
}
