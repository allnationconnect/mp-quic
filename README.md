# Multipath QUIC Implementation (v1.2)

[English](README.md) | [中文](README_zh.md)

This package implements the Multipath Extension for QUIC as specified in [draft-ietf-quic-multipath](https://datatracker.ietf.org/doc/draft-ietf-quic-multipath/).

## Features

- Multiple path management
- Path validation
- Connection ID management
- Packet scheduling
- CUBIC congestion control
- RTT-based path selection
- Automatic retransmission

## Installation

```bash
go get github.com/allnationconnect/mp-quic
```

## Basic Usage

### Creating a Connection

```go
import "github.com/allnationconnect/mp-quic"

// Create a connection with default configuration
conn := mpquic.NewConnection(nil)

// Or with custom configuration
config := &mpquic.Config{
    MaxPaths:              4,
    PathValidationTimeout: 5 * time.Second,
    EnablePathMigration:   true,
    CongestionControl:     "cubic",
}
conn := mpquic.NewConnection(config)
```

### Adding and Managing Paths

```go
// Add a new path
local := &net.UDPAddr{IP: net.ParseIP("192.168.1.1"), Port: 1234}
remote := &net.UDPAddr{IP: net.ParseIP("192.168.1.2"), Port: 5678}
path, err := conn.AddPath(local, remote)
if err != nil {
    log.Error("Failed to add path: %v", err)
}

// Start path validation
validationConfig := mpquic.DefaultValidationConfig()
err = path.StartValidation(validationConfig)
if err != nil {
    log.Error("Failed to start validation: %v", err)
}

// Handle validation response
response := []byte{...} // received from peer
result := path.HandlePathResponse(response)
if result == mpquic.PathValidationSuccess {
    log.Info("Path validation successful")
}
```

### Packet Scheduling

```go
// Create a packet scheduler
scheduler := mpquic.NewPacketScheduler(conn)

// Schedule a packet
frames := []mpquic.Frame{...}
packet, path, err := scheduler.SchedulePacket(frames)
if err != nil {
    log.Error("Failed to schedule packet: %v", err)
}

// Handle acknowledgment
scheduler.HandleAck(packet.Number, time.Now())

// Handle packet loss
scheduler.HandleLoss(packet.Number)
```

### Connection ID Management

```go
// Create a CID manager
cidManager := mpquic.NewCIDManager(4, 4, 8)

// Allocate a local CID
cid, err := cidManager.AllocateLocalCID(pathID)
if err != nil {
    log.Error("Failed to allocate CID: %v", err)
}

// Register a remote CID
err = cidManager.RegisterRemoteCID(remoteCID, pathID)
if err != nil {
    log.Error("Failed to register remote CID: %v", err)
}
```

## Configuration Options

### Path Configuration

- `MaxPaths`: Maximum number of concurrent paths
- `PathValidationTimeout`: Timeout for path validation
- `EnablePathMigration`: Enable/disable path migration

### Validation Configuration

- `Timeout`: Validation timeout duration
- `MaxRetries`: Maximum validation retry attempts
- `ChallengeSize`: Size of validation challenge data

### Congestion Control Configuration

- `Beta`: CUBIC beta parameter
- `C`: CUBIC C parameter
- `InitialWindow`: Initial congestion window
- `MinWindow`: Minimum congestion window
- `MaxWindow`: Maximum congestion window
- `FastConvergence`: Enable/disable fast convergence

## Performance Considerations

1. Path Selection
   - RTT-based scoring
   - Available bandwidth consideration
   - Loss rate impact
   - Path priority

2. Congestion Control
   - CUBIC algorithm implementation
   - TCP friendliness
   - Slow start and congestion avoidance phases

3. Retransmission Strategy
   - Maximum 3 retransmission attempts
   - Alternative path selection for retransmissions
   - Loss rate tracking

## Logging

The implementation uses structured logging with different levels:

- DEBUG: Detailed operation information
- INFO: General operation status
- WARN: Warning conditions
- ERROR: Error conditions

Configure logging level:

```go
log.SetLogLevel("DEBUG")
```

## Testing

Run the test suite:

```bash
go test -v ./mp-quic

# Run benchmarks
go test -bench=. ./mp-quic
```

## Error Handling

Common error types:

- `ErrTooManyPaths`: Maximum path limit reached
- `ErrPathNotFound`: Path not found
- `ErrNoAvailableCIDs`: No available connection IDs
- `ErrTooManyRemoteCIDs`: Remote CID limit reached
- `ErrDuplicateCID`: Duplicate connection ID
- `ErrPathValidationFailed`: Path validation failed
- `ErrPathValidationTimeout`: Path validation timeout

## Contributing

1. Fork the repository
2. Create your feature branch
3. Commit your changes
4. Push to the branch
5. Create a new Pull Request

## License

This project is licensed under the GNU Lesser General Public License v3.0 (LGPL-3.0) - see the [LICENSE](LICENSE) file for details.

### Key points of the license:

- ✅ Commercial use is permitted
- ✅ You can freely use and distribute this software
- ✅ You can modify the software for your needs
- ⚠️ If you modify the software, you must:
  - Make the modified source code available under LGPL-3.0
  - Include the original copyright notice
  - State significant changes made to the software
  - Include the original source code attribution
- ⚠️ You must include:
  - A copy of the license and copyright notice with the code
  - Attribution to this original project 
