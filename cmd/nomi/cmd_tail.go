package main

import (
	"bufio"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// tailCmd follows the SSE event stream live. Useful for debugging
// runs without polling, and for "what's the daemon doing right now?"
//
//	nomi tail
//	nomi tail --filter=approval
func tailCmd(common *commonFlags, args []string) int {
	fs := flag.NewFlagSet("tail", flag.ExitOnError)
	bindCommonFlags(fs, common)
	filter := fs.String("filter", "", "only print events whose type contains this substring")
	_ = fs.Parse(args)

	cli, err := NewClient(common)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	req, _ := http.NewRequest(http.MethodGet, cli.URL+"/events/stream", nil)
	req.Header.Set("Authorization", "Bearer "+cli.Token)
	req.Header.Set("Accept", "text/event-stream")

	// Use a non-timeout client for the long-lived SSE connection. The
	// 30s default on cli.HTTP would slam shut after the first half
	// minute and miss every subsequent event.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "tail: HTTP %d\n", resp.StatusCode)
		return 1
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if *filter != "" && !strings.Contains(payload, *filter) {
			continue
		}
		fmt.Println(payload)
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
