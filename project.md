# GoFlow2 Project Documentation

This document provides an overview of the GoFlow2 project structure, key components, and data processing pipeline.

## Project Overview

GoFlow2 is a NetFlow/IPFIX/sFlow collector written in Go. It's designed to receive flow data from network devices, decode various flow protocols, process the data, and forward it to different destinations (e.g., Kafka, files) in various formats (e.g., JSON, Protobuf).

## Key Directories and Components

### 1. Root Directory (`/`)

Contains top-level configuration and build files:
- `README.md`: Project description, usage, and setup instructions.
- `Makefile`: Build automation tasks.
- `Dockerfile`: Containerization setup.
- `go.mod`, `go.sum`: Go module dependencies.
- Source code directories (`cmd`, `utils`, `decoders`, `producer`, `format`, `transport`).

### 2. `cmd/goflow2`

Contains the main application entry point.
- `main.go`: Initializes the application. Parses command-line flags, sets up logging, configures the processing pipeline (decoder, producer, formatter, transporter) based on flags, starts the UDP listeners (`utils.UDPReceiver`), and launches the main processing goroutines.
- `mapping.yaml`: Default configuration for mapping flow fields in the `producer` stage (specifically for the Protobuf producer).

### 3. `utils/`

Provides core utilities used throughout the application.
- `udp.go`: Implements `UDPReceiver` for listening on UDP ports, receiving packets, and passing them to the processing pipeline. Manages packet pooling (`sync.Pool`) for efficiency.
- `pipe.go`: Defines the `FlowPipe` structure that connects the different stages of the processing pipeline (Producer, Formatter, Transporter) and manages the flow of data between them.
- `mute.go`: Implements `BatchMute` for suppressing repeated error messages to avoid log flooding.
- Other utilities for IP handling, metrics, etc.

### 4. `decoders/`

Contains packages for decoding different flow protocols.
- **`netflow/`**: Handles NetFlow v9 and IPFIX.
  - `packet.go`: Defines structs for packet headers, flow sets, and records.
  - `templates.go`: Implements `NetFlowTemplateSystem` for managing templates (crucial for v9/IPFIX).
  - `netflow.go`: Contains the core decoding logic (`DecodeMessageNetFlow`, `DecodeMessageIPFIX`, `DecodeMessageCommon`, `DecodeTemplateSet`, `DecodeDataSetUsingFields`, etc.) that uses templates to parse data records.
  - `nfv9.go`, `ipfix.go`: Protocol-specific constants and structures.
- **`netflowlegacy/`**: Handles NetFlow v5.
  - `packet.go`: Defines the fixed structure for v5 packets and records.
  - `netflow.go`: Contains the simpler decoding logic for the fixed v5 format.
- **`sflow/`**: Handles sFlow.
  - `packet.go`: Defines the main `Packet` structure containing various sample types (`FlowSample`, `CounterSample`, etc.).
  - `datastructure.go`: Defines structs for the diverse range of sFlow record types.
  - `sflow.go`: Contains the decoding logic that parses the sFlow packet and its various sample/record types based on format codes.

### 5. `producer/`

Responsible for converting decoded protocol-specific data into a common internal format.
- `producer.go`: Defines the `ProducerInterface` with methods `Produce`, `Commit`, and `Close`.
- **`proto/`**: Implements the `ProducerInterface` for the Protobuf format.
  - `proto.go`: Contains the `ProtoProducer` struct and its `Produce` method, which switches on the decoded message type and calls specific processing functions (`ProcessMessageNetFlowLegacy`, `ProcessMessageNetFlowV9Config`, etc.).
  - `producer_nf.go`, `producer_nflegacy.go`, `producer_sf.go`: Contain the logic to map fields from decoded NetFlow/sFlow structs to the common `pb.FlowMessage` struct (defined in `pb/flow.proto`).
  - `config*.go`, `render.go`, `reflect.go`: Handle configuration loading (`mapping.yaml`) and use reflection for dynamic field mapping.
- **`raw/`**: (Not explored in detail) Likely provides a producer that passes through the raw packet data.

### 6. `format/`

Responsible for serializing the common internal format (from the producer) into a specific output format.
- `format.go`: Defines the `FormatDriver` interface (`Prepare`, `Init`, `Format`) and the registration mechanism (`RegisterFormatDriver`, `FindFormat`). The `Format` method takes the internal message and returns a key and the serialized payload as byte slices.
- **`json/`**: Implements `FormatDriver` for JSON output.
- **`binary/`**: Implements `FormatDriver` for Protobuf binary output.
- **`text/`**: Implements `FormatDriver` for plain text output.

### 7. `transport/`

Responsible for sending the serialized data (from the formatter) to the final destination.
- `transport.go`: Defines the `TransportDriver` interface (`Prepare`, `Init`, `Close`, `Send`) and the registration mechanism (`RegisterTransportDriver`, `FindTransport`). The `Send` method takes the key and serialized payload from the formatter.
- **`kafka/`**: Implements `TransportDriver` for sending data to Apache Kafka.
- **`file/`**: Implements `TransportDriver` for writing data to a local file.

## Data Processing Pipeline

1.  **Receive (`utils.UDPReceiver`)**: Listens for UDP packets on configured ports.
2.  **Decode (`decoders/*`)**: Parses the raw packet bytes based on the detected protocol (sFlow, NetFlow v5/v9, IPFIX) into Go structs.
3.  **Produce (`producer/*`)**: Converts the protocol-specific structs into a common internal format (e.g., `pb.FlowMessage`).
4.  **Format (`format/*`)**: Serializes the internal message into the desired output format (e.g., JSON bytes, Protobuf bytes).
5.  **Transport (`transport/*`)**: Sends the serialized bytes to the configured destination (e.g., Kafka topic, file).

This pipeline is configured in `cmd/goflow2/main.go` based on command-line arguments.
