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

// Структура пинга
type PingResult struct {
	Host    string
	Success bool
	Latency time.Duration
	Error   error
}

// TCP доступность
func pingHostTCP(host string, timeout time.Duration, port int) (bool, time.Duration, error) {
	start := time.Now()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), timeout)
	if err != nil {
		return false, 0, err
	}
	defer conn.Close()
	latency := time.Since(start)
	return true, latency, nil
}

// Сложное ICMP доделать потом
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
			Data: []byte("Kim Jong Un"),
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

	err = conn.SetReadDeadline(time.Now().Add(timeout))
	if err != nil {
		return false, 0, fmt.Errorf("set deadline: %w", err)
	}

	reply := make([]byte, 1500)
	n, peer, err := conn.ReadFrom(reply)
	if err != nil {
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			return false, 0, fmt.Errorf("timeout")
		}
		return false, 0, fmt.Errorf("read: %w", err)
	}

	parsedMsg, err := icmp.ParseMessage(ipv4.ICMPTypeEchoReply.Protocol(), reply[:n])
	if err != nil {
		return false, 0, fmt.Errorf("parse: %w", err)
	}

	if parsedMsg.Type == ipv4.ICMPTypeEchoReply {
		echo, ok := parsedMsg.Body.(*icmp.Echo)
		if ok && echo.ID == pid && echo.Seq == seq {
			latency := time.Since(start)
			return true, latency, nil
		}
	}
	return false, 0, fmt.Errorf("unexpected reply from %s", peer)
}

// читаем список хостов из файла
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
	return hosts, scanner.Err()
}

func printTable(results []PingResult, w io.Writer) {
	maxHostLen := 4
	for _, r := range results {
		if len(r.Host) > maxHostLen {
			maxHostLen = len(r.Host)
		}
	}
	hostCol := maxHostLen + 2
	statusCol := 8
	latencyCol := 10

	sepLine := "+" + strings.Repeat("-", hostCol) + "+" + strings.Repeat("-", statusCol) + "+" + strings.Repeat("-", latencyCol) + "+"
	header := fmt.Sprintf("| %-*s | %-*s | %-*s |", hostCol-1, "Host", statusCol-1, "Status", latencyCol-1, "Latency")

	fmt.Fprintln(w, sepLine)
	fmt.Fprintln(w, header)
	fmt.Fprintln(w, sepLine)

	for _, r := range results {
		status := "OK"
		latency := fmt.Sprintf("%dms", r.Latency.Milliseconds())
		if !r.Success {
			status = "FAIL"
			latency = "timeout"
		}
		fmt.Fprintf(w, "| %-*s | %-*s | %-*s |\n", hostCol-1, r.Host, statusCol-1, status, latencyCol-1, latency)
	}
	fmt.Fprintln(w, sepLine)
}

// v тут проблема с параметром НЕ ТРОГАТЬ иишка уже итак все сломала
func runOnce(ctx context.Context, hosts []string, pingFunc func(string) (bool, time.Duration, error)) []PingResult {
	results := make([]PingResult, len(hosts))
	var wg sync.WaitGroup

	for i, host := range hosts {
		wg.Add(1)
		go func(idx int, h string) {
			defer wg.Done()
			success, latency, err := pingFunc(h)
			results[idx] = PingResult{
				Host:    h,
				Success: success,
				Latency: latency,
				Error:   err,
			}
		}(i, host)
	}
	wg.Wait()
	return results
}

func runCounted(ctx context.Context, hosts []string, pingFunc func(string) (bool, time.Duration, error), count int, interval time.Duration, writer io.Writer) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for i := 0; i < count; i++ {
		select {
		case <-ctx.Done():
			fmt.Fprintln(writer, "\nInterrupted.")
			return
		default:
		}

		results := runOnce(ctx, hosts, pingFunc)
		fmt.Fprintln(writer, strings.Repeat("-", 60))
		fmt.Fprintf(writer, "Check #%d at %s\n", i+1, time.Now().Format("2006-01-02 15:04:05"))
		printTable(results, writer)

		if i < count-1 {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}
}

func runMonitor(ctx context.Context, hosts []string, pingFunc func(string) (bool, time.Duration, error), interval time.Duration, writer io.Writer) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(writer, "\nMonitoring stopped.")
			return
		case <-ticker.C:
			results := runOnce(ctx, hosts, pingFunc)
			fmt.Fprintln(writer, strings.Repeat("-", 60))
			fmt.Fprintf(writer, "Check at %s\n", time.Now().Format("2006-01-02 15:04:05"))
			printTable(results, writer)
		}
	}
}

func main() {
	fPtr := flag.String("f", "", "Path to file with hosts list (required)")
	monitorPtr := flag.Bool("monitor", false, "Continuous monitoring mode")
	countPtr := flag.Int("c", 0, "Number of checks to perform (0 = run once)")
	intervalPtr := flag.Int("interval", 1, "Interval in seconds between checks (for -c and -monitor)")
	outputPtr := flag.String("output", "", "Path to output file (optional)")
	timeoutPtr := flag.Duration("timeout", 2*time.Second, "Timeout for each ping")
	portPtr := flag.Int("port", 80, "Port to ping (TCP mode only)")
	icmpPtr := flag.Bool("icmp", false, "Use ICMP ping (requires privileges)")

	flag.Parse()

	if *fPtr == "" {
		fmt.Fprintln(os.Stderr, "Ошибка: требуется указать файл -f")
		flag.Usage()
		os.Exit(1)
	}

	if *monitorPtr && *countPtr > 0 {
		fmt.Fprintln(os.Stderr, "Ошибка! -monitor и -c несовместимы")
		os.Exit(1)
	}

	// Чтение хостов из файла
	hosts, err := readHosts(*fPtr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка чтения: %v\n", err)
		os.Exit(1)
	}
	if len(hosts) == 0 {
		fmt.Fprintln(os.Stderr, "Ошибка. Нет файла с хостами")
		os.Exit(1)
	}

	outputWriters := []io.Writer{os.Stdout}
	var outputFile *os.File
	if *outputPtr != "" {
		outputFile, err = os.Create(*outputPtr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Ошибка при создании файла вывода: %v\n", err)
			os.Exit(1)
		}
		defer outputFile.Close()
		outputWriters = append(outputWriters, outputFile)
	}
	multiWriter := io.MultiWriter(outputWriters...)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Fprintln(os.Stderr, "\nShutdown...")
		cancel()
	}()

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

	interval := time.Duration(*intervalPtr) * time.Second

	if *monitorPtr {
		runMonitor(ctx, hosts, pingFunc, interval, multiWriter)
	} else if *countPtr > 0 {
		runCounted(ctx, hosts, pingFunc, *countPtr, interval, multiWriter)
	} else {
		results := runOnce(ctx, hosts, pingFunc)
		printTable(results, multiWriter)
	}
}
