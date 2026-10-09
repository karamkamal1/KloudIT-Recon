// Command recon-gateway serves the KloudIT Recon web client, authenticates
// users and relays streams between browsers and host agents. It is designed to
// run in a small container or VM (e.g. a Proxmox LXC).
//
//	recon-gateway [flags] [serve]
//	recon-gateway [flags] user add <name> [-admin]     (password read from stdin)
//	recon-gateway [flags] user passwd <name>           (password read from stdin)
//	recon-gateway [flags] user reset-2fa <name>
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/auth"
	"github.com/karamkamal1/kloudit-recon/internal/gateway"
	"github.com/karamkamal1/kloudit-recon/web"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// envList returns the comma-separated values of environment variable k (the
// installed service's /etc/kloudit-recon/gateway.env), for the repeatable
// flags.
func envList(k string) []string {
	var out []string
	for _, v := range strings.Split(os.Getenv(k), ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func main() {
	listen := flag.String("listen", env("RECON_LISTEN", ":8443"), "address for HTTPS (TCP) and HTTP/3 + host tunnels (UDP)")
	data := flag.String("data", env("RECON_DATA", "./data"), "data directory (state, certificates, audit log)")
	cert := flag.String("cert", env("RECON_CERT", ""), "TLS certificate (PEM); default: generated private CA")
	key := flag.String("key", env("RECON_KEY", ""), "TLS key (PEM)")
	public := flag.String("public-addr", env("RECON_PUBLIC_ADDR", ""), "host:port the host agents use to reach this gateway")
	relayPorts := flag.String("relay-ports", env("RECON_RELAY_PORTS", gateway.DefaultRelayPorts),
		"UDP ports for relayed WebTransport sessions, one per session (e.g. 8444-8459; off = QUIC splice relay on the main port only)")
	webDir := flag.String("web", env("RECON_WEB_DIR", ""), "serve the web client from this directory (development)")
	verbose := flag.Bool("v", false, "debug logging")
	var names, proxies multiFlag
	flag.Var(&names, "name", "extra DNS name or IP for the generated certificate (repeatable)")
	flag.Var(&proxies, "trust-proxy", "address or CIDR of a reverse proxy whose X-Forwarded-For is trusted (repeatable; RECON_TRUST_PROXY, comma-separated)")
	flag.Parse()
	names = append(names, envList("RECON_NAMES")...)
	proxies = append(proxies, envList("RECON_TRUST_PROXY")...)

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	args := flag.Args()
	if len(args) > 0 && args[0] == "user" {
		userCmd(*data, args[1:])
		return
	}
	if len(args) > 0 && args[0] == "version" {
		fmt.Println(gateway.Version)
		return
	}
	if len(args) > 0 && args[0] != "serve" {
		fmt.Fprintln(os.Stderr, "unknown command:", args[0])
		os.Exit(2)
	}

	var webFS fs.FS = web.FS()
	if *webDir != "" {
		webFS = os.DirFS(*webDir)
	}
	srv, err := gateway.New(gateway.Config{
		Listen: *listen, DataDir: *data, Names: names, CertFile: *cert, KeyFile: *key,
		PublicAddr: *public, TrustProxy: proxies, RelayPorts: *relayPorts, Web: webFS,
	}, log)
	if err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(ctx); err != nil {
		log.Error("gateway stopped", "err", err)
		os.Exit(1)
	}
}

func readPassword() string {
	if fi, _ := os.Stdin.Stat(); fi.Mode()&os.ModeCharDevice != 0 {
		fmt.Fprint(os.Stderr, "Password (input is visible; prefer piping it): ")
	}
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimRight(line, "\r\n")
}

// userCmd manages accounts offline (stop the gateway first: it keeps state in memory).
func userCmd(dataDir string, args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: recon-gateway user add|passwd|reset-2fa <name> [-admin]")
		os.Exit(2)
	}
	store, err := gateway.OpenStore(dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	name := args[1]
	switch args[0] {
	case "add", "passwd":
		pw := readPassword()
		if len([]rune(pw)) < 10 {
			fmt.Fprintln(os.Stderr, "password must be at least 10 characters")
			os.Exit(1)
		}
		hash, err := auth.HashPassword(pw)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		admin := len(args) > 2 && args[2] == "-admin"
		first := store.UserCount() == 0 // before UpdateUser, which holds the store lock
		err = store.UpdateUser(name, func(u *gateway.User, exists bool) error {
			if args[0] == "add" && exists {
				return fmt.Errorf("user %s exists", name)
			}
			if args[0] == "passwd" && !exists {
				return fmt.Errorf("no user %s", name)
			}
			u.PasswordHash = hash
			if args[0] == "add" {
				u.Admin = admin || first
				u.Created = time.Now().UTC()
			}
			return nil
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("ok")
	case "reset-2fa":
		err := store.UpdateUser(name, func(u *gateway.User, exists bool) error {
			if !exists {
				return fmt.Errorf("no user %s", name)
			}
			u.TOTPSecret, u.TOTPLast = "", 0
			return nil
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("ok")
	default:
		fmt.Fprintln(os.Stderr, "unknown user command")
		os.Exit(2)
	}
}
