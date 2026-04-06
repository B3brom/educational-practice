package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

type PingResult struct {
	Timestamp time.Time
	Host      string
	Success   bool
	Latency   time.Duration
	Error     error
	Round     int
	Total     int
}

func pingHostTCP(host string, timeout time.Duration, port int) (bool, time.Duration, error) {
	start := time.Now()

	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false, 0, err
	}
	defer conn.Close()

	return true, time.Since(start), nil
}

func pingHostICMP(host string, timeout time.Duration) (bool, time.Duration, error) {
	ipAddr, err := net.ResolveIPAddr("ip4", host)
	if err != nil {
		return false, 0, fmt.Errorf("resolve: %w", err)
	}

	conn, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return false, 0, fmt.Errorf("icmp listen: %w", err)
	}
	defer conn.Close()

	pid := os.Getpid() & 0xffff
	seq := 1

	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{
			ID:   pid,
			Seq:  seq,
			Data: []byte("ping"),
		},
	}

	msgBytes, err := msg.Marshal(nil)
	if err != nil {
		return false, 0, fmt.Errorf("marshal: %w", err)
	}

	start := time.Now()

	_, err = conn.WriteTo(msgBytes, ipAddr)
	if err != nil {
		return false, 0, fmt.Errorf("write: %w", err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return false, 0, fmt.Errorf("set deadline: %w", err)
	}

	reply := make([]byte, 1500)
	n, _, err := conn.ReadFrom(reply)
	if err != nil {
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			return false, 0, nil
		}
		return false, 0, fmt.Errorf("read: %w", err)
	}

	parsedMsg, err := icmp.ParseMessage(ipv4.ICMPTypeEchoReply.Protocol(), reply[:n])
	if err != nil {
		return false, 0, fmt.Errorf("parse: %w", err)
	}

	if parsedMsg.Type != ipv4.ICMPTypeEchoReply {
		return false, 0, fmt.Errorf("unexpected ICMP type")
	}

	echo, ok := parsedMsg.Body.(*icmp.Echo)
	if !ok {
		return false, 0, fmt.Errorf("unexpected ICMP body")
	}

	if echo.ID != pid || echo.Seq != seq {
		return false, 0, fmt.Errorf("unexpected echo reply")
	}

	return true, time.Since(start), nil
}

func readHosts(filename string) ([]string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var hosts []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			hosts = append(hosts, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return hosts, nil
}

func statusText(res PingResult) string {
	if res.Success {
		return "OK"
	}
	return "Timeout"
}

func latencyText(res PingResult) string {
	if res.Success {
		return fmt.Sprintf("%dms", res.Latency.Milliseconds())
	}
	return "0ms"
}

func printTable(results []PingResult, title string, w io.Writer) {
	if len(results) == 0 {
		return
	}

	maxHostLen := len("Host")
	maxStatusLen := len("Status")
	maxLatencyLen := len("Latency")

	for _, r := range results {
		if len(r.Host) > maxHostLen {
			maxHostLen = len(r.Host)
		}
		if l := len(statusText(r)); l > maxStatusLen {
			maxStatusLen = l
		}
		if l := len(latencyText(r)); l > maxLatencyLen {
			maxLatencyLen = l
		}
	}

	const tsLen = len("2006-01-02 15:04:05")

	tsSep := strings.Repeat("-", tsLen+2)
	hostSep := strings.Repeat("-", maxHostLen+2)
	statusSep := strings.Repeat("-", maxStatusLen+2)
	latencySep := strings.Repeat("-", maxLatencyLen+2)

	sepLine := "+" + tsSep + "+" + hostSep + "+" + statusSep + "+" + latencySep + "+"

	if title != "" {
		fmt.Fprintln(w, title)
	}
	fmt.Fprintln(w, sepLine)
	fmt.Fprintf(w, "| %-19s | %-*s | %-*s | %-*s |\n",
		"Timestamp",
		maxHostLen, "Host",
		maxStatusLen, "Status",
		maxLatencyLen, "Latency",
	)
	fmt.Fprintln(w, sepLine)

	for _, r := range results {
		fmt.Fprintf(w, "| %-19s | %-*s | %-*s | %-*s |\n",
			r.Timestamp.Format("2006-01-02 15:04:05"),
			maxHostLen, r.Host,
			maxStatusLen, statusText(r),
			maxLatencyLen, latencyText(r),
		)
	}

	fmt.Fprintln(w, sepLine)
}

func printResultLog(w io.Writer, res PingResult) {
	status := "OK"
	latency := fmt.Sprintf("%dms", res.Latency.Milliseconds())

	if !res.Success {
		status = "FAIL"
		latency = "0ms"
	}

	fmt.Fprintf(w, "%s | %s | %s | %s\n",
		res.Timestamp.Format("2006-01-02 15:04:05"),
		res.Host,
		status,
		latency,
	)
}

func orderResults(hosts []string, batch map[string]PingResult) []PingResult {
	out := make([]PingResult, 0, len(hosts))
	for _, h := range hosts {
		if r, ok := batch[h]; ok {
			out = append(out, r)
		}
	}
	return out
}

func main() {
	fPtr := flag.String("f", "", "Path to file with hosts list (required)")
	monitorPtr := flag.Bool("monitor", false, "Continuous monitoring mode")
	countPtr := flag.Int("c", 0, "Number of checks to perform (0 = run once)")
	intervalPtr := flag.Int("interval", 1, "Interval in seconds")
	outputPtr := flag.String("output", "", "Path to output file (optional)")
	timeoutPtr := flag.Duration("timeout", 2*time.Second, "Timeout for each ping")
	portPtr := flag.Int("port", 80, "Port for TCP ping")
	icmpPtr := flag.Bool("icmp", false, "Use ICMP ping")

	flag.Parse()

	if *fPtr == "" {
		fmt.Fprintln(os.Stderr, "Ошибка: требуется указать файл -f")
		os.Exit(1)
	}

	if *monitorPtr && *countPtr > 0 {
		fmt.Fprintln(os.Stderr, "Ошибка: флаги -monitor и -c несовместимы")
		os.Exit(1)
	}

	hosts, err := readHosts(*fPtr)
	if err != nil || len(hosts) == 0 {
		fmt.Fprintln(os.Stderr, "Ошибка чтения файла хостов")
		os.Exit(1)
	}

	var logWriter io.Writer
	if *outputPtr != "" {
		file, err := os.Create(*outputPtr)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Ошибка создания файла лога")
			os.Exit(1)
		}
		defer file.Close()
		logWriter = file
	}

	var pingFunc func(string) (bool, time.Duration, error)
	if *icmpPtr {
		pingFunc = func(host string) (bool, time.Duration, error) {
			return pingHostICMP(host, *timeoutPtr)
		}
	} else {
		pingFunc = func(host string) (bool, time.Duration, error) {
			return pingHostTCP(host, *timeoutPtr, *portPtr)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigChan)

	go func() {
		<-sigChan
		fmt.Fprintln(os.Stderr, "\nShutdown...")
		cancel()
	}()

	interval := time.Duration(*intervalPtr) * time.Second
	resultsChan := make(chan PingResult, len(hosts)*2)

	var printerWG sync.WaitGroup
	printerWG.Add(1)

	go func() {
		defer printerWG.Done()

		batches := make(map[int]map[string]PingResult)

		for res := range resultsChan {

			if logWriter != nil {
				printResultLog(logWriter, res)
			}

			if res.Total == 0 {
				fmt.Printf("%s | %s | %s | %s\n",
					res.Timestamp.Format("15:04:05"),
					res.Host,
					statusText(res),
					latencyText(res),
				)
				continue
			}

			batch := batches[res.Round]
			if batch == nil {
				batch = make(map[string]PingResult, len(hosts))
				batches[res.Round] = batch
			}

			batch[res.Host] = res

			if len(batch) == len(hosts) {
				ordered := orderResults(hosts, batch)

				title := fmt.Sprintf(
					"Check %d/%d at %s",
					res.Round,
					res.Total,
					time.Now().Format("2006-01-02 15:04:05"),
				)

				fmt.Println(strings.Repeat("-", 60))
				printTable(ordered, title, os.Stdout)

				delete(batches, res.Round)
			}
		}
	}()

	if *countPtr > 0 {
		for attempt := 1; attempt <= *countPtr; attempt++ {
			var wg sync.WaitGroup

			for _, host := range hosts {
				wg.Add(1)

				go func(h string, round int) {
					defer wg.Done()

					success, latency, err := pingFunc(h)
					res := PingResult{
						Timestamp: time.Now(),
						Host:      h,
						Success:   success,
						Latency:   latency,
						Error:     err,
						Round:     round,
						Total:     *countPtr,
					}

					select {
					case resultsChan <- res:
					case <-ctx.Done():
					}
				}(host, attempt)
			}

			wg.Wait()

			if attempt < *countPtr {
				select {
				case <-ctx.Done():
					close(resultsChan)
					printerWG.Wait()
					return
				case <-time.After(interval):
				}
			}
		}

		close(resultsChan)
		printerWG.Wait()
		return
	}

	if *monitorPtr {
		var wg sync.WaitGroup

		for _, host := range hosts {
			wg.Add(1)

			go func(h string) {
				defer wg.Done()

				ticker := time.NewTicker(interval)
				defer ticker.Stop()

				round := 1

				doPing := func(r int) {
					success, latency, err := pingFunc(h)
					res := PingResult{
						Timestamp: time.Now(),
						Host:      h,
						Success:   success,
						Latency:   latency,
						Error:     err,
						Round:     r,
						Total:     0,
					}

					select {
					case resultsChan <- res:
					case <-ctx.Done():
					}
				}

				doPing(round)
				round++

				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						doPing(round)
						round++
					}
				}
			}(host)
		}

		<-ctx.Done()
		wg.Wait()
		close(resultsChan)
		printerWG.Wait()
		return
	}

	var wg sync.WaitGroup
	for _, host := range hosts {
		wg.Add(1)

		go func(h string) {
			defer wg.Done()

			success, latency, err := pingFunc(h)
			res := PingResult{
				Timestamp: time.Now(),
				Host:      h,
				Success:   success,
				Latency:   latency,
				Error:     err,
				Round:     1,
				Total:     1,
			}

			select {
			case resultsChan <- res:
			case <-ctx.Done():
			}
		}(host)
	}

	wg.Wait()
	close(resultsChan)
	printerWG.Wait()
}
