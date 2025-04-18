package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Configuration for the aggregator
type Config struct {
	InputLogFile      string
	OutputLogFile     string
	AggregationPeriod time.Duration
}

// NetFlowRecord represents a parsed NetFlow record from the JSON log
type NetFlowRecord struct {
	Type               string   `json:"type"`
	TimeReceivedNs     int64    `json:"time_received_ns"`
	SrcAddr            string   `json:"src_addr"`
	DstAddr            string   `json:"dst_addr"`
	SrcPort            int      `json:"src_port"`
	DstPort            int      `json:"dst_port"`
	PostNatSrcAddr     string   `json:"post_nat_src_addr"`
	PostNatDstAddr     string   `json:"post_nat_dst_addr"`
	PostSrcMac         string   `json:"post_src_mac"`
	PostDstMac         string   `json:"post_dst_mac"`
	Bytes              int64    `json:"bytes"`
	Packets            int64    `json:"packets"`
	Proto              string   `json:"proto"`
	InIf               int      `json:"in_if"`
	OutIf              int      `json:"out_if"`
	SamplingRate       int      `json:"sampling_rate"`
	TimeFlowStartNs    int64    `json:"time_flow_start_ns"`
	TimeFlowEndNs      int64    `json:"time_flow_end_ns"`
}

// AggregatedRecord represents an aggregated NetFlow record
type AggregatedRecord struct {
	AggregationKey    string    `json:"aggregation_key"`
	SrcAddr           string    `json:"src_addr"`
	DstAddr           string    `json:"dst_addr"`
	Port              int       `json:"port"`
	PostSrcMac        string    `json:"post_src_mac"`
	PostDstMac        string    `json:"post_dst_mac"`
	TotalBytes        int64     `json:"total_bytes"`
	TotalPackets      int64     `json:"total_packets"`
	FlowCount         int       `json:"flow_count"`
	FirstSeenTime     time.Time `json:"first_seen_time"`
	LastSeenTime      time.Time `json:"last_seen_time"`
	Proto             string    `json:"proto"`
	Direction         string    `json:"direction"` // "inbound" or "outbound"
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

// determinePort determines which port to use based on traffic direction
func (a *Aggregator) determinePort(record *NetFlowRecord) (int, string) {
	srcIsPrivate := a.isPrivateIP(record.SrcAddr)
	dstIsPrivate := a.isPrivateIP(a.determineEffectiveDstAddr(record))

	// If both source and destination are private, we'll skip this record
	if srcIsPrivate && dstIsPrivate {
		return 0, ""
	}

	// Outbound traffic: source is private, destination is public
	if srcIsPrivate && !dstIsPrivate {
		return record.DstPort, "outbound"
	}

	// Inbound traffic: source is public, destination is private
	if !srcIsPrivate && dstIsPrivate {
		return record.SrcPort, "inbound"
	}

	// Both public (unusual case) - default to destination port
	return record.DstPort, "unknown"
}

// createAggregationKey creates a unique key for aggregation
func (a *Aggregator) createAggregationKey(record *NetFlowRecord, port int) string {
	effectiveDstAddr := a.determineEffectiveDstAddr(record)
	return fmt.Sprintf("%s|%s|%d|%s|%s", 
		record.SrcAddr, 
		effectiveDstAddr, 
		port, 
		record.PostSrcMac, 
		record.PostDstMac)
}

// processRecord processes a single NetFlow record
func (a *Aggregator) processRecord(record *NetFlowRecord) {
	port, direction := a.determinePort(record)
	if direction == "" {
		// Skip records where both IPs are private
		return
	}

	key := a.createAggregationKey(record, port)
	effectiveDstAddr := a.determineEffectiveDstAddr(record)
	
	a.mutex.Lock()
	defer a.mutex.Unlock()

	// Create or update the aggregated record
	if _, exists := a.aggregatedFlows[key]; !exists {
		a.aggregatedFlows[key] = &AggregatedRecord{
			AggregationKey: key,
			SrcAddr:        record.SrcAddr,
			DstAddr:        effectiveDstAddr,
			Port:           port,
			PostSrcMac:     record.PostSrcMac,
			PostDstMac:     record.PostDstMac,
			TotalBytes:     record.Bytes,
			TotalPackets:   record.Packets,
			FlowCount:      1,
			FirstSeenTime:  time.Unix(0, record.TimeReceivedNs),
			LastSeenTime:   time.Unix(0, record.TimeReceivedNs),
			Proto:          record.Proto,
			Direction:      direction,
		}
	} else {
		// Update existing record
		aggRecord := a.aggregatedFlows[key]
		aggRecord.TotalBytes += record.Bytes
		aggRecord.TotalPackets += record.Packets
		aggRecord.FlowCount++
		
		// Update first seen time if this record is older
		recordTime := time.Unix(0, record.TimeReceivedNs)
		if recordTime.Before(aggRecord.FirstSeenTime) {
			aggRecord.FirstSeenTime = recordTime
		}
		
		// Update last seen time if this record is newer
		if recordTime.After(aggRecord.LastSeenTime) {
			aggRecord.LastSeenTime = recordTime
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

// writeAggregatedData writes the aggregated data to the output file
func (a *Aggregator) writeAggregatedData() error {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	if len(a.aggregatedFlows) == 0 {
		return nil // Nothing to write
	}

	// Ensure directory exists
	dir := filepath.Dir(a.config.OutputLogFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %v", err)
	}

	file, err := os.OpenFile(a.config.OutputLogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open output file: %v", err)
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	for _, record := range a.aggregatedFlows {
		if err := encoder.Encode(record); err != nil {
			return fmt.Errorf("failed to encode record: %v", err)
		}
	}

	// Clear the aggregated flows after writing
	a.aggregatedFlows = make(map[string]*AggregatedRecord)

	return nil
}

// Run starts the aggregation process
func (a *Aggregator) Run() {
	ticker := time.NewTicker(a.config.AggregationPeriod)
	defer ticker.Stop()

	log.Printf("Starting NetFlow aggregator. Input: %s, Output: %s, Period: %v",
		a.config.InputLogFile, a.config.OutputLogFile, a.config.AggregationPeriod)

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
	flag.StringVar(&config.OutputLogFile, "output", "/var/log/aggregated_flow.log", "Output aggregated log file")
	periodMinutes := flag.Int("period", 5, "Aggregation period in minutes")
	flag.Parse()

	config.AggregationPeriod = time.Duration(*periodMinutes) * time.Minute

	// Create and run the aggregator
	aggregator := NewAggregator(config)
	aggregator.Run()
}
