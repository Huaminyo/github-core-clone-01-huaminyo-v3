// github-core-clone v2 — the entrypoint's Go core: a standalone
// GitHub-compatible git forge (smart-HTTP git, REST v3, GraphQL, MCP) in one
// process, standard library only. main.ts (the trial entrypoint) launches
// this binary; this file is only the lifecycle below.
//
// The entry <-> env lifecycle, as env/src/wire.ts settles it: bind an
// ephemeral port, tell the env the port it bound, then poll until the env
// says the trial is over and exit 0.
//
//   1. MAKEFUN_ENV_URL names the env's origin (http://store:8080 in a trial container).
//      Without it there is nothing to talk to, so the program runs in dev mode
//      instead: a fixed port (PORT, default 8080) with no env lifecycle.
//   2. Listen on an ephemeral port (trial) — the kernel picks a free port.
//      Never bind 8080 in a trial: the env owns it.
//   3. POST $MAKEFUN_ENV_URL/register {"port": N}. The env records the address
//      the request came from plus N as the base URL and dials it from then on.
//      The first register wins; a 409 means another entry already registered,
//      and this process keeps waiting like any other.
//   4. GET $MAKEFUN_ENV_URL/done until the body is {"done": true}. The env
//      answers at once ({"done": false} while the suite runs), so this is a
//      poll, not a long-held request. A failed or slow poll is retried with a
//      short exponential backoff; the process never exits before the env says done.
//   5. Close the listener and exit 0.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

const requestTimeout = 30 * time.Second
const retryInitial = 250 * time.Millisecond
const retryMax = 2 * time.Millisecond * 1000

func retryDelay(attempt int) time.Duration {
	d := retryInitial
	for i := 0; i < attempt && i < 8; i++ {
		d *= 2
	}
	if d > retryMax {
		return retryMax
	}
	return d
}

// register POSTs the bound port until the env accepts it. 200 is the normal
// answer; 409 means a first register already won. 400 and 405 are contract
// errors and end the program; anything else is retried.
func register(client *http.Client, envURL string, port int) error {
	body, _ := json.Marshal(map[string]int{"port": port})
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest("POST", envURL+"/register", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("content-type", "application/json")
		resp, err := client.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 || resp.StatusCode == 409 {
				return nil
			}
			if resp.StatusCode == 400 || resp.StatusCode == 405 {
				return fmt.Errorf("register answered HTTP %d", resp.StatusCode)
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

// waitForDone polls GET /done until it answers {"done":true}; never returns before that.
func waitForDone(client *http.Client, envURL string) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest("GET", envURL+"/done", nil)
		if err == nil {
			resp, err := client.Do(req)
			if err == nil {
				data, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					var parsed struct {
						Done bool `json:"done"`
					}
					if json.Unmarshal(data, &parsed) == nil && parsed.Done {
						return
					}
				}
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

func main() {
	// The trial pod allows 2 CPUs and 1024 MiB. Go 1.24's default
	// GOMAXPROCS counts the host's CPUs, not the cgroup limit, so a big
	// host would oversubscribe the 2-CPU quota with spinning Ps and GC
	// workers; pin it to the limit. Live data is a few tens of MB, so a
	// higher GC percent trades idle heap for far fewer GC cycles.
	runtime.GOMAXPROCS(2)
	debug.SetGCPercent(200)
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	a := newApp()
	if envURL == "" {
		// Dev mode: the same server on a fixed port, without the env lifecycle.
		//   MAKEFUN_SHOP_TOKEN=test-token PORT=8080 ./forge
		port := 8080
		if raw := os.Getenv("PORT"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n <= 0 || n > 65535 {
				fmt.Fprintf(os.Stderr, "[dev] PORT must be a TCP port number, got %q\n", raw)
				os.Exit(1)
			}
			port = n
		}
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
		if err != nil {
			fmt.Fprintf(os.Stderr, "[dev] listen: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("[dev] listening on http://127.0.0.1:%d\n", port)
		a.serve(ln)
		return
	}

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[entry] listen: %v\n", err)
		os.Exit(1)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		fmt.Fprintf(os.Stderr, "[entry] the server did not bind an ephemeral TCP port\n")
		os.Exit(1)
	}
	fmt.Printf("[entry] listening on port %d\n", addr.Port)
	go a.serve(ln)

	client := &http.Client{Timeout: requestTimeout}
	if err := register(client, envURL, addr.Port); err != nil {
		fmt.Fprintf(os.Stderr, "[entry] %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("[entry] registered; waiting for the env to say done\n")

	waitForDone(client, envURL)
	ln.Close()
	fmt.Printf("[entry] done\n")
}
