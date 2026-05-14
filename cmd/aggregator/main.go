package main

import (
	"bufio"
	"compress/gzip"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// hexToMAC converts a hex string like "04f41c4fbba1" to "04:f4:1c:4f:bb:a1"
func hexToMAC(h string) string {
	h = strings.TrimPrefix(h, "0x")
	if len(h) != 12 {
		return h
	}
	b, err := hex.DecodeString(h)
	if err != nil {
		return h
	}
	return net.HardwareAddr(b).String()
}

// Configuration for the aggregator
type Config struct {
	InputLogFile      string
	OutputDir         string
	AggregationPeriod time.Duration
}

// NetFlowRecord represents a parsed NetFlow record from the JSON log.
// Field names match the prod goflow2-mapping.yaml output.
type NetFlowRecord struct {
	Type               string `json:"type"`
	TimeReceivedNs     string `json:"time_received_ns"`
	SamplerAddress     string `json:"sampler_address"`
	SrcAddr            string `json:"src_addr"`
	DstAddr            string `json:"dst_addr"`
	SrcPort            int    `json:"src_port"`
	DstPort            int    `json:"dst_port"`
	PostNatSrcAddr     string `json:"post_nat_source_ipv4_address"`
	PostNatDstAddr     string `json:"post_nat_destination_ipv4_address"`
	PostSourceMac      string `json:"post_source_mac_address"`
	PostDestinationMac string `json:"post_destination_mac_address"`
	InDstMac           string `json:"in_dst_mac"`
	Bytes              int64  `json:"bytes"`
	Packets            int64  `json:"packets"`
	Proto              string `json:"proto"`
	InIf               int    `json:"in_if"`
	OutIf              int    `json:"out_if"`
	SamplingRate       int    `json:"sampling_rate"`
	TimeFlowStartNs    string `json:"time_flow_start_ns"`
	TimeFlowEndNs      string `json:"time_flow_end_ns"`
}

// AggregatedRecord represents an aggregated NetFlow record
type AggregatedRecord struct {
	SrcAddr        string    `json:"src_addr"`
	DstAddr        string    `json:"dst_addr"`
	Port           int       `json:"port"`
	Direction      string    `json:"direction"`
	WanMac         string    `json:"wan_mac"`
	LanMac         string    `json:"lan_mac"`
	TotalBytes     int64     `json:"total_bytes"`
	TotalPackets   int64     `json:"total_packets"`
	FlowCount      int       `json:"flow_count"`
	Proto          string    `json:"proto"`
	SamplerAddress string    `json:"sampler_address"`
	FirstSeenTime  time.Time `json:"first_seen_time"`
	LastSeenTime   time.Time `json:"last_seen_time"`
}

// Aggregator handles the aggregation of NetFlow records
type Aggregator struct {
	config           Config
	aggregatedFlows  map[string]*AggregatedRecord
	mutex            sync.Mutex
	lastProcessedPos int64
	privateNetworks  []*net.IPNet
}

// NewAggregator creates a new Aggregator instance
func NewAggregator(config Config) *Aggregator {
	// Initialize private networks CIDR blocks
	_, privateNet1, _ := net.ParseCIDR("10.0.0.0/8")
	_, privateNet2, _ := net.ParseCIDR("172.16.0.0/12")
	_, privateNet3, _ := net.ParseCIDR("192.168.0.0/16")
	_, privateNet4, _ := net.ParseCIDR("fc00::/7") // IPv6 ULA

	return &Aggregator{
		config:           config,
		aggregatedFlows:  make(map[string]*AggregatedRecord),
		privateNetworks:  []*net.IPNet{privateNet1, privateNet2, privateNet3, privateNet4},
		lastProcessedPos: 0,
	}
}

// isPrivateIP checks if an IP address is private
func (a *Aggregator) isPrivateIP(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}

	for _, privateNet := range a.privateNetworks {
		if privateNet.Contains(ip) {
			return true
		}
	}
	return false
}

// determineEffectiveDstAddr determines the effective destination address
// based on the logic: use post_nat_dst_addr if available, otherwise use dst_addr
func (a *Aggregator) determineEffectiveDstAddr(record *NetFlowRecord) string {
	if record.PostNatDstAddr != "" && record.PostNatDstAddr != "0.0.0.0" {
		return record.PostNatDstAddr
	}
	return record.DstAddr
}

// determineDirection determines traffic direction and the service port.
func (a *Aggregator) determineDirection(record *NetFlowRecord) (int, string) {
	srcIsPrivate := a.isPrivateIP(record.SrcAddr)
	dstIsPrivate := a.isPrivateIP(a.determineEffectiveDstAddr(record))

	// Both private = internal traffic, skip
	if srcIsPrivate && dstIsPrivate {
		return 0, ""
	}

	// Outbound: source is private, destination is public
	if srcIsPrivate && !dstIsPrivate {
		return record.DstPort, "outbound"
	}

	// Inbound: source is public, destination is private
	if !srcIsPrivate && dstIsPrivate {
		return record.SrcPort, "inbound"
	}

	// Both public (unusual) - default to destination port
	return record.DstPort, "unknown"
}

// deriveWanLanMac computes wan_mac and lan_mac from post_source_mac (field 81)
// and in_dst_mac (field 80) based on traffic direction, matching prod logic:
//   - field 81 (post_source_mac) = egress interface MAC
//   - field 80 (in_dst_mac)      = ingress interface MAC
//   - Outbound: ingress=LAN, egress=WAN → wan=field81, lan=field80
//   - Inbound:  ingress=WAN, egress=LAN → wan=field80, lan=field81
func deriveWanLanMac(direction, postSourceMac, inDstMac string) (wanMac, lanMac string) {
	switch direction {
	case "outbound":
		return postSourceMac, inDstMac
	case "inbound":
		return inDstMac, postSourceMac
	default:
		return "", ""
	}
}

// createAggregationKey creates a unique key for aggregation
func createAggregationKey(srcAddr, dstAddr string, port int, direction, wanMac, lanMac string) string {
	return fmt.Sprintf("%s|%s|%d|%s|%s|%s",
		srcAddr,
		dstAddr,
		port,
		direction,
		wanMac,
		lanMac)
}

// processRecord processes a single NetFlow record
func (a *Aggregator) processRecord(record *NetFlowRecord) {
	port, direction := a.determineDirection(record)
	if direction != "inbound" && direction != "outbound" {
		return
	}

	effectiveDstAddr := a.determineEffectiveDstAddr(record)
	wanMac, lanMac := deriveWanLanMac(direction, hexToMAC(record.PostSourceMac), hexToMAC(record.InDstMac))
	key := createAggregationKey(record.SrcAddr, effectiveDstAddr, port, direction, wanMac, lanMac)

	a.mutex.Lock()
	defer a.mutex.Unlock()

	recordTime, _ := time.Parse(time.RFC3339Nano, record.TimeReceivedNs)

	if existing, ok := a.aggregatedFlows[key]; !ok {
		a.aggregatedFlows[key] = &AggregatedRecord{
			SrcAddr:        record.SrcAddr,
			DstAddr:        effectiveDstAddr,
			Port:           port,
			Direction:      direction,
			WanMac:         wanMac,
			LanMac:         lanMac,
			TotalBytes:     record.Bytes,
			TotalPackets:   record.Packets,
			FlowCount:      1,
			Proto:          record.Proto,
			SamplerAddress: record.SamplerAddress,
			FirstSeenTime:  recordTime,
			LastSeenTime:   recordTime,
		}
	} else {
		existing.TotalBytes += record.Bytes
		existing.TotalPackets += record.Packets
		existing.FlowCount++

		if recordTime.Before(existing.FirstSeenTime) {
			existing.FirstSeenTime = recordTime
		}
		if recordTime.After(existing.LastSeenTime) {
			existing.LastSeenTime = recordTime
		}
	}
}

// processLogFile processes the NetFlow log file
func (a *Aggregator) processLogFile() error {
	file, err := os.Open(a.config.InputLogFile)
	if err != nil {
		return fmt.Errorf("failed to open input log file: %v", err)
	}
	defer file.Close()

	// Seek to the last processed position
	if _, err := file.Seek(a.lastProcessedPos, 0); err != nil {
		return fmt.Errorf("failed to seek to last position: %v", err)
	}

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}

		var record NetFlowRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			log.Printf("Error parsing JSON: %v, line: %s", err, line)
			continue
		}

		a.processRecord(&record)
	}

	// Update the last processed position
	pos, err := file.Seek(0, 1) // Get current position
	if err == nil {
		a.lastProcessedPos = pos
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error reading log file: %v", err)
	}

	return nil
}

// writeAggregatedData writes aggregated data as an atomic gzip file.
// Writes to a .tmp file first, then renames to .json.gz so consumers
// never see incomplete files.
func (a *Aggregator) writeAggregatedData() error {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	if len(a.aggregatedFlows) == 0 {
		return nil
	}

	if err := os.MkdirAll(a.config.OutputDir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %v", err)
	}

	ts := time.Now().UTC().Format("20060102_150405")
	finalPath := fmt.Sprintf("%s/aggregated.%s.json.gz", a.config.OutputDir, ts)
	tmpPath := finalPath + ".tmp"

	file, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("failed to create temp file: %v", err)
	}

	gz, _ := gzip.NewWriterLevel(file, gzip.BestSpeed)
	encoder := json.NewEncoder(gz)
	for _, record := range a.aggregatedFlows {
		if err := encoder.Encode(record); err != nil {
			gz.Close()
			file.Close()
			os.Remove(tmpPath)
			return fmt.Errorf("failed to encode record: %v", err)
		}
	}

	if err := gz.Close(); err != nil {
		file.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("failed to close gzip writer: %v", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("failed to sync file: %v", err)
	}
	file.Close()

	if err := os.Rename(tmpPath, finalPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to rename temp file: %v", err)
	}

	log.Printf("Wrote %s (%d records)", finalPath, len(a.aggregatedFlows))
	a.aggregatedFlows = make(map[string]*AggregatedRecord)

	// Truncate the raw flow log to free disk space
	if err := os.Truncate(a.config.InputLogFile, 0); err != nil {
		log.Printf("Warning: failed to truncate input log: %v", err)
	}
	a.lastProcessedPos = 0

	return nil
}

// Run starts the aggregation process
func (a *Aggregator) Run() {
	ticker := time.NewTicker(a.config.AggregationPeriod)
	defer ticker.Stop()

	log.Printf("Starting NetFlow aggregator. Input: %s, OutputDir: %s, Period: %v",
		a.config.InputLogFile, a.config.OutputDir, a.config.AggregationPeriod)

	for range ticker.C {
		if err := a.processLogFile(); err != nil {
			log.Printf("Error processing log file: %v", err)
		}

		if err := a.writeAggregatedData(); err != nil {
			log.Printf("Error writing aggregated data: %v", err)
		}
	}
}

func main() {
	var config Config

	// Parse command line flags
	flag.StringVar(&config.InputLogFile, "input", "/var/log/flow.log", "Input NetFlow log file")
	flag.StringVar(&config.OutputDir, "output-dir", "/var/log/flows", "Output directory for gzip files")
	periodMinutes := flag.Int("period", 5, "Aggregation period in minutes")
	flag.Parse()

	config.AggregationPeriod = time.Duration(*periodMinutes) * time.Minute

	// Create and run the aggregator
	aggregator := NewAggregator(config)
	aggregator.Run()
}
