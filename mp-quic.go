package mpquic

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/allnationconnect/websocket_tunnel/log"
)

// PathID represents a unique identifier for a path
type PathID uint32

// PathState represents the state of a path
type PathState int

const (
	PathStateInitial PathState = iota
	PathStateValidating
	PathStateActive
	PathStateStandby
	PathStateDraining
	PathStateClosed
)

// PathMetrics contains path-specific metrics
type PathMetrics struct {
	mu            sync.RWMutex
	MinRTT        time.Duration
	SmoothedRTT   time.Duration
	RTTVar        time.Duration
	BytesSent     uint64
	BytesReceived uint64
	LossRate      float64
	LastUpdate    time.Time
}

func (m *PathMetrics) UpdateRTT(rtt time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.SmoothedRTT == 0 {
		m.SmoothedRTT = rtt
		m.RTTVar = rtt / 2
		m.MinRTT = rtt
	} else {
		rttVar := m.RTTVar
		m.RTTVar = (3*rttVar + time.Duration(math.Abs(float64(rtt-m.SmoothedRTT)))) / 4
		m.SmoothedRTT = (7*m.SmoothedRTT + rtt) / 8
		if rtt < m.MinRTT {
			m.MinRTT = rtt
		}
	}
	m.LastUpdate = time.Now()
}

func (m *PathMetrics) AddBytesSent(bytes uint64) {
	if bytes > 0 {
		current := atomic.LoadUint64(&m.BytesSent)
		for {
			if current > math.MaxUint64-bytes {
				if atomic.CompareAndSwapUint64(&m.BytesSent, current, math.MaxUint64) {
					return
				}
			} else {
				if atomic.CompareAndSwapUint64(&m.BytesSent, current, current+bytes) {
					return
				}
			}
			current = atomic.LoadUint64(&m.BytesSent)
		}
	}
}

func (m *PathMetrics) AddBytesReceived(bytes uint64) {
	if bytes > 0 {
		current := atomic.LoadUint64(&m.BytesReceived)
		for {
			if current > math.MaxUint64-bytes {
				if atomic.CompareAndSwapUint64(&m.BytesReceived, current, math.MaxUint64) {
					return
				}
			} else {
				if atomic.CompareAndSwapUint64(&m.BytesReceived, current, current+bytes) {
					return
				}
			}
			current = atomic.LoadUint64(&m.BytesReceived)
		}
	}
}

func (m *PathMetrics) UpdateLossRate(newLossRate float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.LossRate = newLossRate
}

func (m *PathMetrics) GetMetrics() (time.Duration, time.Duration, time.Duration, uint64, uint64, float64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.MinRTT, m.SmoothedRTT, m.RTTVar,
		atomic.LoadUint64(&m.BytesSent),
		atomic.LoadUint64(&m.BytesReceived),
		m.LossRate
}

// Path represents a single path in a multipath QUIC connection
type Path struct {
	mu     sync.RWMutex
	ID     PathID
	State  PathState
	Local  net.Addr
	Remote net.Addr

	// Path priority (0 is highest)
	Priority uint8

	// Connection IDs
	LocalCID  []byte
	RemoteCID []byte

	// Path-specific packet number space
	nextPacketNumber uint64
	largestAcked     uint64

	// ACK tracking
	largestAckedPacketNumber uint64
	largestSentPacketNumber  uint64
	unackedPackets           map[uint64]time.Time

	// Path validation
	challengeData     []byte
	validationTimer   *time.Timer
	validationStatus  bool
	validationRetries int

	// Congestion control
	congestionWindow   uint64
	bytesInFlight      uint64
	slowStartThreshold uint64

	// Path metrics
	metrics PathMetrics

	// Path state management
	lastStateChange time.Time
	stateHistory    []PathState

	// 连续丢包计数
	consecutiveLosses int32
}

func (p *Path) GetNextPacketNumber() uint64 {
	next := atomic.AddUint64(&p.nextPacketNumber, 1)
	if next == math.MaxUint64 {
		// Reset packet number if it reaches maximum
		atomic.StoreUint64(&p.nextPacketNumber, 0)
		return 0
	}
	return next
}

func (p *Path) AddBytesInFlight(delta int64) uint64 {
	if delta >= 0 {
		return atomic.AddUint64(&p.bytesInFlight, uint64(delta))
	}
	// Handle negative delta
	return atomic.AddUint64(&p.bytesInFlight, ^uint64(-delta-1))
}

func (p *Path) GetBytesInFlight() uint64 {
	return atomic.LoadUint64(&p.bytesInFlight)
}

func (p *Path) UpdateCongestionWindow(newWindow uint64) {
	atomic.StoreUint64(&p.congestionWindow, newWindow)
}

func (p *Path) GetCongestionWindow() uint64 {
	return atomic.LoadUint64(&p.congestionWindow)
}

func (p *Path) IncrementConsecutiveLosses() int32 {
	return atomic.AddInt32(&p.consecutiveLosses, 1)
}

func (p *Path) ResetConsecutiveLosses() {
	atomic.StoreInt32(&p.consecutiveLosses, 0)
}

func (p *Path) GetConsecutiveLosses() int32 {
	return atomic.LoadInt32(&p.consecutiveLosses)
}

// Connection represents a multipath QUIC connection
type Connection struct {
	// Path management
	paths       map[PathID]*Path
	activePaths []PathID
	pathsMutex  sync.RWMutex

	// Connection configuration
	config *Config

	// TLS configuration
	tlsConfig *tls.Config

	// Connection state
	closed bool

	mu sync.RWMutex

	cidManager *CIDManager
}

// Config contains the configuration for a multipath QUIC connection
type Config struct {
	MaxPaths              uint32
	PathValidationTimeout time.Duration
	EnablePathMigration   bool
	CongestionControl     string
}

// NewConnection creates a new multipath QUIC connection
func NewConnection(config *Config) *Connection {
	if config == nil {
		config = &Config{
			MaxPaths:              8,
			PathValidationTimeout: 5 * time.Second,
			EnablePathMigration:   true,
			CongestionControl:     "cubic",
		}
	}

	return &Connection{
		paths:       make(map[PathID]*Path),
		activePaths: make([]PathID, 0),
		config:      config,
	}
}

// AddPath adds a new path to the connection
func (c *Connection) AddPath(local, remote net.Addr) (*Path, error) {
	c.pathsMutex.Lock()
	defer c.pathsMutex.Unlock()

	if len(c.paths) >= int(c.config.MaxPaths) {
		log.Warn("Failed to add path: too many paths (max: %d)", c.config.MaxPaths)
		return nil, ErrTooManyPaths
	}

	path := &Path{
		ID:     c.generatePathID(),
		State:  PathStateInitial,
		Local:  local,
		Remote: remote,
	}

	c.paths[path.ID] = path
	log.Info("Added new path %d: %s -> %s", path.ID, local.String(), remote.String())
	return path, nil
}

// RemovePath removes a path from the connection
func (c *Connection) RemovePath(id PathID) error {
	c.pathsMutex.Lock()
	defer c.pathsMutex.Unlock()

	if path, exists := c.paths[id]; exists {
		path.State = PathStateClosed
		delete(c.paths, id)
		return nil
	}

	return ErrPathNotFound
}

// generatePathID generates a new unique path ID
func (c *Connection) generatePathID() PathID {
	// Simple implementation - should be improved for production use
	return PathID(len(c.paths))
}

// Errors
var (
	ErrTooManyPaths = errors.New("too many paths")
	ErrPathNotFound = errors.New("path not found")
)

// Frame types for multipath QUIC
const (
	FrameTypePATH_CHALLENGE    uint64 = 0x1a
	FrameTypePATH_RESPONSE     uint64 = 0x1b
	FrameTypePATH_STATUS       uint64 = 0x1c
	FrameTypePATH_ABANDON      uint64 = 0x1d
	FrameTypeMAX_PATH_ID       uint64 = 0x1e
	FrameTypeADD_ADDRESS       uint64 = 0x1f
	FrameTypeREMOVE_ADDRESS    uint64 = 0x20
	FrameTypeUNIFLOWS          uint64 = 0x21
	FrameTypePATH_CIDS_BLOCKED uint64 = 0x22
)

// Frame interface for all multipath QUIC frames
type Frame interface {
	Type() uint64
	Encode() ([]byte, error)
	Decode([]byte) error
}

// PathChallengeFrame is sent to validate a path
type PathChallengeFrame struct {
	Data [8]byte
}

func (f *PathChallengeFrame) Type() uint64 {
	return FrameTypePATH_CHALLENGE
}

// Encode encodes a PathChallengeFrame into bytes
func (f *PathChallengeFrame) Encode() ([]byte, error) {
	b := make([]byte, 9)
	b[0] = byte(FrameTypePATH_CHALLENGE)
	copy(b[1:], f.Data[:])
	return b, nil
}

// Decode decodes a PathChallengeFrame from bytes
func (f *PathChallengeFrame) Decode(b []byte) error {
	if len(b) < 9 {
		return ErrFrameTooShort
	}
	copy(f.Data[:], b[1:9])
	return nil
}

// PathResponseFrame is sent in response to a PATH_CHALLENGE
type PathResponseFrame struct {
	Data [8]byte
}

func (f *PathResponseFrame) Type() uint64 {
	return FrameTypePATH_RESPONSE
}

// Encode encodes a PathResponseFrame into bytes
func (f *PathResponseFrame) Encode() ([]byte, error) {
	b := make([]byte, 9)
	b[0] = byte(FrameTypePATH_RESPONSE)
	copy(b[1:], f.Data[:])
	return b, nil
}

// Decode decodes a PathResponseFrame from bytes
func (f *PathResponseFrame) Decode(b []byte) error {
	if len(b) < 9 {
		return ErrFrameTooShort
	}
	copy(f.Data[:], b[1:9])
	return nil
}

// PathStatusFrame indicates the status of a path
type PathStatusFrame struct {
	PathID     PathID
	Status     PathState
	SequenceNo uint64
}

func (f *PathStatusFrame) Type() uint64 {
	return FrameTypePATH_STATUS
}

// Encode encodes a PathStatusFrame into bytes
func (f *PathStatusFrame) Encode() ([]byte, error) {
	b := make([]byte, 17)
	b[0] = byte(FrameTypePATH_STATUS)
	binary.BigEndian.PutUint32(b[1:], uint32(f.PathID))
	b[5] = byte(f.Status)
	binary.BigEndian.PutUint64(b[9:], f.SequenceNo)
	return b, nil
}

// Decode decodes a PathStatusFrame from bytes
func (f *PathStatusFrame) Decode(b []byte) error {
	if len(b) < 17 {
		return ErrFrameTooShort
	}
	f.PathID = PathID(binary.BigEndian.Uint32(b[1:]))
	f.Status = PathState(b[5])
	f.SequenceNo = binary.BigEndian.Uint64(b[9:])
	return nil
}

// PathAbandonFrame indicates that a path is being abandoned
type PathAbandonFrame struct {
	PathID PathID
	Reason uint64
}

func (f *PathAbandonFrame) Type() uint64 {
	return FrameTypePATH_ABANDON
}

// Encode encodes a PathAbandonFrame into bytes
func (f *PathAbandonFrame) Encode() ([]byte, error) {
	b := make([]byte, 13)
	b[0] = byte(FrameTypePATH_ABANDON)
	binary.BigEndian.PutUint32(b[1:], uint32(f.PathID))
	binary.BigEndian.PutUint64(b[5:], f.Reason)
	return b, nil
}

// Decode decodes a PathAbandonFrame from bytes
func (f *PathAbandonFrame) Decode(b []byte) error {
	if len(b) < 13 {
		return ErrFrameTooShort
	}
	f.PathID = PathID(binary.BigEndian.Uint32(b[1:]))
	f.Reason = binary.BigEndian.Uint64(b[5:])
	return nil
}

// MaxPathIDFrame advertises the maximum path ID
type MaxPathIDFrame struct {
	MaxPathID PathID
}

func (f *MaxPathIDFrame) Type() uint64 {
	return FrameTypeMAX_PATH_ID
}

// Encode encodes a MaxPathIDFrame into bytes
func (f *MaxPathIDFrame) Encode() ([]byte, error) {
	b := make([]byte, 5)
	b[0] = byte(FrameTypeMAX_PATH_ID)
	binary.BigEndian.PutUint32(b[1:], uint32(f.MaxPathID))
	return b, nil
}

// Decode decodes a MaxPathIDFrame from bytes
func (f *MaxPathIDFrame) Decode(b []byte) error {
	if len(b) < 5 {
		return ErrFrameTooShort
	}
	f.MaxPathID = PathID(binary.BigEndian.Uint32(b[1:]))
	return nil
}

// AddAddressFrame advertises a new local address
type AddAddressFrame struct {
	AddressID uint64
	Address   net.Addr
}

func (f *AddAddressFrame) Type() uint64 {
	return FrameTypeADD_ADDRESS
}

// RemoveAddressFrame removes a previously advertised address
type RemoveAddressFrame struct {
	AddressID uint64
}

func (f *RemoveAddressFrame) Type() uint64 {
	return FrameTypeREMOVE_ADDRESS
}

// UniflowsFrame advertises the number of unidirectional flows
type UniflowsFrame struct {
	ReceivingUniflows uint64
	SendingUniflows   uint64
	ReceivingRequired uint64
	SendingRequired   uint64
}

func (f *UniflowsFrame) Type() uint64 {
	return FrameTypeUNIFLOWS
}

// PathCIDsBlockedFrame indicates that no more CIDs are available
type PathCIDsBlockedFrame struct {
	PathID PathID
}

func (f *PathCIDsBlockedFrame) Type() uint64 {
	return FrameTypePATH_CIDS_BLOCKED
}

// Error types for frame handling
var (
	ErrInvalidFrameType = errors.New("invalid frame type")
	ErrFrameTooShort    = errors.New("frame too short")
	ErrInvalidData      = errors.New("invalid frame data")
)

// Helper function to create a new Frame from its type
func NewFrame(frameType uint64) (Frame, error) {
	switch frameType {
	case FrameTypePATH_CHALLENGE:
		return &PathChallengeFrame{}, nil
	case FrameTypePATH_RESPONSE:
		return &PathResponseFrame{}, nil
	case FrameTypePATH_STATUS:
		return &PathStatusFrame{}, nil
	case FrameTypePATH_ABANDON:
		return &PathAbandonFrame{}, nil
	case FrameTypeMAX_PATH_ID:
		return &MaxPathIDFrame{}, nil
	case FrameTypeADD_ADDRESS:
		return &AddAddressFrame{}, nil
	case FrameTypeREMOVE_ADDRESS:
		return &RemoveAddressFrame{}, nil
	case FrameTypeUNIFLOWS:
		return &UniflowsFrame{}, nil
	case FrameTypePATH_CIDS_BLOCKED:
		return &PathCIDsBlockedFrame{}, nil
	default:
		return nil, ErrInvalidFrameType
	}
}

// Encode encodes an AddAddressFrame into bytes
func (f *AddAddressFrame) Encode() ([]byte, error) {
	// Address encoding format:
	// 1 byte: address type (1 = IPv4, 2 = IPv6)
	// n bytes: address bytes
	// 2 bytes: port
	addrBytes, addrType, err := encodeAddress(f.Address)
	if err != nil {
		return nil, err
	}

	b := make([]byte, 9+len(addrBytes))
	b[0] = byte(FrameTypeADD_ADDRESS)
	binary.BigEndian.PutUint64(b[1:], f.AddressID)
	b[9] = addrType
	copy(b[10:], addrBytes)
	return b, nil
}

// Decode decodes an AddAddressFrame from bytes
func (f *AddAddressFrame) Decode(b []byte) error {
	if len(b) < 10 {
		return ErrFrameTooShort
	}
	f.AddressID = binary.BigEndian.Uint64(b[1:])
	addr, err := decodeAddress(b[9:])
	if err != nil {
		return err
	}
	f.Address = addr
	return nil
}

// Encode encodes a RemoveAddressFrame into bytes
func (f *RemoveAddressFrame) Encode() ([]byte, error) {
	b := make([]byte, 9)
	b[0] = byte(FrameTypeREMOVE_ADDRESS)
	binary.BigEndian.PutUint64(b[1:], f.AddressID)
	return b, nil
}

// Decode decodes a RemoveAddressFrame from bytes
func (f *RemoveAddressFrame) Decode(b []byte) error {
	if len(b) < 9 {
		return ErrFrameTooShort
	}
	f.AddressID = binary.BigEndian.Uint64(b[1:])
	return nil
}

// Encode encodes a UniflowsFrame into bytes
func (f *UniflowsFrame) Encode() ([]byte, error) {
	b := make([]byte, 33)
	b[0] = byte(FrameTypeUNIFLOWS)
	binary.BigEndian.PutUint64(b[1:], f.ReceivingUniflows)
	binary.BigEndian.PutUint64(b[9:], f.SendingUniflows)
	binary.BigEndian.PutUint64(b[17:], f.ReceivingRequired)
	binary.BigEndian.PutUint64(b[25:], f.SendingRequired)
	return b, nil
}

// Decode decodes a UniflowsFrame from bytes
func (f *UniflowsFrame) Decode(b []byte) error {
	if len(b) < 33 {
		return ErrFrameTooShort
	}
	f.ReceivingUniflows = binary.BigEndian.Uint64(b[1:])
	f.SendingUniflows = binary.BigEndian.Uint64(b[9:])
	f.ReceivingRequired = binary.BigEndian.Uint64(b[17:])
	f.SendingRequired = binary.BigEndian.Uint64(b[25:])
	return nil
}

// Encode encodes a PathCIDsBlockedFrame into bytes
func (f *PathCIDsBlockedFrame) Encode() ([]byte, error) {
	b := make([]byte, 5)
	b[0] = byte(FrameTypePATH_CIDS_BLOCKED)
	binary.BigEndian.PutUint32(b[1:], uint32(f.PathID))
	return b, nil
}

// Decode decodes a PathCIDsBlockedFrame from bytes
func (f *PathCIDsBlockedFrame) Decode(b []byte) error {
	if len(b) < 5 {
		return ErrFrameTooShort
	}
	f.PathID = PathID(binary.BigEndian.Uint32(b[1:]))
	return nil
}

// Helper functions for address encoding/decoding
func encodeAddress(addr net.Addr) ([]byte, byte, error) {
	switch a := addr.(type) {
	case *net.TCPAddr:
		if ip4 := a.IP.To4(); ip4 != nil {
			b := make([]byte, 6)
			copy(b[:4], ip4)
			binary.BigEndian.PutUint16(b[4:], uint16(a.Port))
			return b, 1, nil
		}
		b := make([]byte, 18)
		copy(b[:16], a.IP)
		binary.BigEndian.PutUint16(b[16:], uint16(a.Port))
		return b, 2, nil
	case *net.UDPAddr:
		if ip4 := a.IP.To4(); ip4 != nil {
			b := make([]byte, 6)
			copy(b[:4], ip4)
			binary.BigEndian.PutUint16(b[4:], uint16(a.Port))
			return b, 1, nil
		}
		b := make([]byte, 18)
		copy(b[:16], a.IP)
		binary.BigEndian.PutUint16(b[16:], uint16(a.Port))
		return b, 2, nil
	default:
		return nil, 0, errors.New("unsupported address type")
	}
}

func decodeAddress(b []byte) (net.Addr, error) {
	if len(b) < 1 {
		return nil, ErrFrameTooShort
	}

	addrType := b[0]
	b = b[1:]

	switch addrType {
	case 1: // IPv4
		if len(b) < 6 {
			return nil, ErrFrameTooShort
		}
		return &net.UDPAddr{
			IP:   net.IP(b[:4]),
			Port: int(binary.BigEndian.Uint16(b[4:])),
		}, nil
	case 2: // IPv6
		if len(b) < 18 {
			return nil, ErrFrameTooShort
		}
		return &net.UDPAddr{
			IP:   net.IP(b[:16]),
			Port: int(binary.BigEndian.Uint16(b[16:])),
		}, nil
	default:
		return nil, errors.New("unsupported address type")
	}
}

// PathOption represents a function that can be used to configure a Path
type PathOption func(*Path)

// WithPriority sets the priority of a path
func WithPriority(priority uint8) PathOption {
	return func(p *Path) {
		p.Priority = priority
	}
}

// WithLocalCID sets the local connection ID of a path
func WithLocalCID(cid []byte) PathOption {
	return func(p *Path) {
		p.LocalCID = cid
	}
}

// WithRemoteCID sets the remote connection ID of a path
func WithRemoteCID(cid []byte) PathOption {
	return func(p *Path) {
		p.RemoteCID = cid
	}
}

// NewPath creates a new path with the given options
func NewPath(id PathID, local, remote net.Addr, opts ...PathOption) *Path {
	p := &Path{
		ID:     id,
		State:  PathStateInitial,
		Local:  local,
		Remote: remote,

		unackedPackets: make(map[uint64]time.Time),
		stateHistory:   make([]PathState, 0),

		congestionWindow:   initialCongestionWindow,
		slowStartThreshold: maxCongestionWindow,

		metrics: PathMetrics{
			MinRTT: time.Duration(math.MaxInt64),
		},

		lastStateChange: time.Now(),
	}

	for _, opt := range opts {
		opt(p)
	}

	return p
}

// UpdateState updates the path state and records the state change
func (p *Path) UpdateState(newState PathState) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.State != newState {
		p.stateHistory = append(p.stateHistory, p.State)
		p.State = newState
		p.lastStateChange = time.Now()
	}
}

// UpdateMetrics updates path metrics with a new RTT sample
func (p *Path) UpdateMetrics(rtt time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	p.metrics.LastUpdate = now

	p.metrics.UpdateRTT(rtt)
}

// IsActive returns true if the path is in active state
func (p *Path) IsActive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.State == PathStateActive
}

// IsValidated returns true if the path has been validated
func (p *Path) IsValidated() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.validationStatus
}

// Constants for congestion control
const (
	initialCongestionWindow = 14720    // 10 full-sized packets
	maxCongestionWindow     = 14720000 // ~10MB
	minCongestionWindow     = 2940     // 2 full-sized packets
)

// PathValidationResult represents the result of a path validation attempt
type PathValidationResult int

const (
	PathValidationSuccess PathValidationResult = iota
	PathValidationTimeout
	PathValidationFailed
)

// ValidationConfig contains configuration for path validation
type ValidationConfig struct {
	Timeout       time.Duration
	MaxRetries    int
	ChallengeSize int
}

// DefaultValidationConfig returns the default validation configuration
func DefaultValidationConfig() ValidationConfig {
	return ValidationConfig{
		Timeout:       3 * time.Second,
		MaxRetries:    3,
		ChallengeSize: 8,
	}
}

// StartValidation initiates the path validation process
func (p *Path) StartValidation(config ValidationConfig) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.validationStatus {
		log.Debug("Path %d already validated", p.ID)
		return nil
	}

	if p.validationTimer != nil {
		p.validationTimer.Stop()
	}

	p.challengeData = make([]byte, config.ChallengeSize)
	if _, err := rand.Read(p.challengeData); err != nil {
		log.Error("Failed to generate challenge data for path %d: %v", p.ID, err)
		return fmt.Errorf("failed to generate challenge data: %w", err)
	}

	p.validationRetries = 0
	p.validationTimer = time.AfterFunc(config.Timeout, func() {
		p.handleValidationTimeout(config)
	})

	log.Info("Started validation for path %d, timeout: %v", p.ID, config.Timeout)
	return nil
}

// HandlePathResponse handles a PATH_RESPONSE frame
func (p *Path) HandlePathResponse(response []byte) PathValidationResult {
	p.mu.Lock()

	if p.validationStatus {
		p.mu.Unlock()
		log.Debug("Path %d already validated", p.ID)
		return PathValidationSuccess
	}

	if p.challengeData == nil {
		p.mu.Unlock()
		log.Warn("No challenge data for path %d", p.ID)
		return PathValidationFailed
	}

	if !bytes.Equal(response, p.challengeData) {
		p.mu.Unlock()
		log.Warn("Challenge response mismatch for path %d", p.ID)
		return PathValidationFailed
	}

	if p.validationTimer != nil {
		p.validationTimer.Stop()
		p.validationTimer = nil
	}

	p.validationStatus = true
	p.challengeData = nil
	p.mu.Unlock()

	p.UpdateState(PathStateActive)
	log.Info("Path %d validation successful", p.ID)
	return PathValidationSuccess
}

// handleValidationTimeout handles a validation timeout
func (p *Path) handleValidationTimeout(config ValidationConfig) {
	p.mu.Lock()

	if p.validationStatus {
		p.mu.Unlock()
		return // Already validated
	}

	p.validationRetries++
	shouldDrain := p.validationRetries >= config.MaxRetries
	p.mu.Unlock()

	if shouldDrain {
		p.UpdateState(PathStateDraining)
		return
	}

	// Generate new challenge data and restart timer
	if _, err := rand.Read(p.challengeData); err != nil {
		p.UpdateState(PathStateDraining)
		return
	}

	p.validationTimer = time.AfterFunc(config.Timeout, func() {
		p.handleValidationTimeout(config)
	})
}

// InvalidatePath marks the path as invalid and triggers cleanup
func (p *Path) InvalidatePath() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.validationTimer != nil {
		p.validationTimer.Stop()
		p.validationTimer = nil
	}

	p.validationStatus = false
	p.challengeData = nil
	p.UpdateState(PathStateDraining)
}

// ValidationStatus returns the current validation status and number of retries
func (p *Path) ValidationStatus() (bool, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.validationStatus, p.validationRetries
}

// Error types for path validation
var (
	ErrPathValidationFailed  = errors.New("path validation failed")
	ErrPathValidationTimeout = errors.New("path validation timeout")
)

// CIDManager manages connection IDs for a QUIC connection
type CIDManager struct {
	// Local CIDs
	localCIDs     map[string]PathID // CID -> PathID mapping
	availableCIDs [][]byte          // Available CIDs for new paths

	// Remote CIDs
	remoteCIDs  map[string]PathID
	retiredCIDs map[string]time.Time

	// Configuration
	maxLocalCIDs  uint32
	maxRemoteCIDs uint32
	cidLen        int

	mu sync.RWMutex
}

// NewCIDManager creates a new CID manager
func NewCIDManager(maxLocal, maxRemote uint32, cidLen int) *CIDManager {
	return &CIDManager{
		localCIDs:     make(map[string]PathID),
		remoteCIDs:    make(map[string]PathID),
		retiredCIDs:   make(map[string]time.Time),
		availableCIDs: make([][]byte, 0),
		maxLocalCIDs:  maxLocal,
		maxRemoteCIDs: maxRemote,
		cidLen:        cidLen,
	}
}

// GenerateNewCID generates a new connection ID
func (cm *CIDManager) GenerateNewCID() ([]byte, error) {
	cid := make([]byte, cm.cidLen)
	if _, err := rand.Read(cid); err != nil {
		return nil, fmt.Errorf("failed to generate CID: %w", err)
	}
	return cid, nil
}

// AllocateLocalCID allocates a local CID for a path
func (cm *CIDManager) AllocateLocalCID(pathID PathID) ([]byte, error) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	// Check if we have available pre-generated CIDs
	if len(cm.availableCIDs) > 0 {
		cid := cm.availableCIDs[0]
		cm.availableCIDs = cm.availableCIDs[1:]
		cm.localCIDs[string(cid)] = pathID
		return cid, nil
	}

	// Generate new CID if we haven't reached the limit
	if uint32(len(cm.localCIDs)) >= cm.maxLocalCIDs {
		return nil, ErrNoAvailableCIDs
	}

	cid, err := cm.GenerateNewCID()
	if err != nil {
		return nil, err
	}

	cm.localCIDs[string(cid)] = pathID
	return cid, nil
}

// RegisterRemoteCID registers a remote CID for a path
func (cm *CIDManager) RegisterRemoteCID(cid []byte, pathID PathID) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if uint32(len(cm.remoteCIDs)) >= cm.maxRemoteCIDs {
		return ErrTooManyRemoteCIDs
	}

	cidStr := string(cid)
	if _, exists := cm.remoteCIDs[cidStr]; exists {
		return ErrDuplicateCID
	}

	cm.remoteCIDs[cidStr] = pathID
	return nil
}

// RetireLocalCID retires a local CID
func (cm *CIDManager) RetireLocalCID(cid []byte) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	cidStr := string(cid)
	delete(cm.localCIDs, cidStr)
	cm.retiredCIDs[cidStr] = time.Now()
}

// RetireRemoteCID retires a remote CID
func (cm *CIDManager) RetireRemoteCID(cid []byte) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	cidStr := string(cid)
	delete(cm.remoteCIDs, cidStr)
	cm.retiredCIDs[cidStr] = time.Now()
}

// GetPathByLocalCID returns the path ID associated with a local CID
func (cm *CIDManager) GetPathByLocalCID(cid []byte) (PathID, bool) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	pathID, exists := cm.localCIDs[string(cid)]
	return pathID, exists
}

// GetPathByRemoteCID returns the path ID associated with a remote CID
func (cm *CIDManager) GetPathByRemoteCID(cid []byte) (PathID, bool) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	pathID, exists := cm.remoteCIDs[string(cid)]
	return pathID, exists
}

// PrepareCIDs generates and stores a batch of CIDs for future use
func (cm *CIDManager) PrepareCIDs(count int) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	for i := 0; i < count; i++ {
		cid, err := cm.GenerateNewCID()
		if err != nil {
			return err
		}
		cm.availableCIDs = append(cm.availableCIDs, cid)
	}
	return nil
}

// CleanupRetiredCIDs removes retired CIDs older than the specified duration
func (cm *CIDManager) CleanupRetiredCIDs(maxAge time.Duration) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	now := time.Now()
	for cid, retireTime := range cm.retiredCIDs {
		if now.Sub(retireTime) > maxAge {
			delete(cm.retiredCIDs, cid)
		}
	}
}

// Error types for CID management
var (
	ErrNoAvailableCIDs   = errors.New("no available connection IDs")
	ErrTooManyRemoteCIDs = errors.New("too many remote connection IDs")
	ErrDuplicateCID      = errors.New("duplicate connection ID")
)

// PacketNumber represents a packet number in QUIC
type PacketNumber uint64

// PacketType represents the type of a QUIC packet
type PacketType uint8

const (
	PacketTypeInitial PacketType = iota
	PacketTypeHandshake
	PacketTypeOneRTT
	PacketTypeRetry
	PacketTypeVersionNegotiation
)

// Packet represents a QUIC packet
type Packet struct {
	Type     PacketType
	Number   PacketNumber
	PathID   PathID
	Frames   []Frame
	Length   uint64
	SendTime time.Time
	AckTime  time.Time
	Retries  uint32
	ECN      uint8 // Explicit Congestion Notification
}

// PacketScheduler is responsible for scheduling packets across multiple paths
type PacketScheduler struct {
	conn *Connection

	// Scheduling configuration
	minRTOTimeout time.Duration
	maxRTOTimeout time.Duration
	rttAlpha      float64 // Smoothing factor for SRTT calculation
	rttBeta       float64 // Smoothing factor for RTTVAR calculation

	// Packet tracking
	sentPackets     map[PacketNumber]*Packet
	lostPackets     map[PacketNumber]*Packet
	retransmissions map[PacketNumber][]PacketNumber // original -> retransmissions

	mu sync.RWMutex
}

// NewPacketScheduler creates a new packet scheduler
func NewPacketScheduler(conn *Connection) *PacketScheduler {
	return &PacketScheduler{
		conn:            conn,
		minRTOTimeout:   200 * time.Millisecond,
		maxRTOTimeout:   1500 * time.Millisecond,
		rttAlpha:        0.125,
		rttBeta:         0.25,
		sentPackets:     make(map[PacketNumber]*Packet),
		lostPackets:     make(map[PacketNumber]*Packet),
		retransmissions: make(map[PacketNumber][]PacketNumber),
	}
}

type LogMetrics struct {
	Timestamp        time.Time
	PathID           PathID
	Event            string
	RTT              time.Duration
	SmoothedRTT      time.Duration
	BytesInFlight    uint64
	CongestionWindow uint64
	LossRate         float64
	PacketNumber     PacketNumber
}

func (ps *PacketScheduler) logPathMetrics(path *Path, event string, packetNumber PacketNumber) {
	metrics := LogMetrics{
		Timestamp:        time.Now(),
		PathID:           path.ID,
		Event:            event,
		BytesInFlight:    path.GetBytesInFlight(),
		CongestionWindow: path.GetCongestionWindow(),
		PacketNumber:     packetNumber,
	}

	path.metrics.mu.RLock()
	metrics.RTT = path.metrics.MinRTT
	metrics.SmoothedRTT = path.metrics.SmoothedRTT
	metrics.LossRate = path.metrics.LossRate
	path.metrics.mu.RUnlock()

	log.Debug("Path Metrics - Event: %s, Path: %d, Packet: %d, RTT: %v, SRTT: %v, InFlight: %d/%d, Loss: %.2f%%",
		metrics.Event,
		metrics.PathID,
		metrics.PacketNumber,
		metrics.RTT,
		metrics.SmoothedRTT,
		metrics.BytesInFlight,
		metrics.CongestionWindow,
		metrics.LossRate*100)
}

// SchedulePacket schedules a packet for transmission on the best available path
func (ps *PacketScheduler) SchedulePacket(frames []Frame) (*Packet, *Path, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	path := ps.selectBestPath()
	if path == nil {
		log.Warn("No available path for packet scheduling")
		return nil, nil, ErrNoAvailablePath
	}

	packetNumber := path.GetNextPacketNumber()
	packet := &Packet{
		Type:     PacketTypeOneRTT,
		Number:   PacketNumber(packetNumber),
		PathID:   path.ID,
		Frames:   frames,
		SendTime: time.Now(),
	}

	path.AddBytesInFlight(int64(packet.Length))
	ps.sentPackets[packet.Number] = packet

	ps.logPathMetrics(path, "PacketScheduled", packet.Number)
	return packet, path, nil
}

// selectBestPath selects the best path for packet transmission
func (ps *PacketScheduler) selectBestPath() *Path {
	var bestPath *Path
	var bestScore float64

	for _, pathID := range ps.conn.activePaths {
		path := ps.conn.paths[pathID]
		if !path.IsActive() || !path.IsValidated() {
			continue
		}

		// Calculate path score based on multiple factors
		score := ps.calculatePathScore(path)
		if score > bestScore {
			bestScore = score
			bestPath = path
		}
	}

	return bestPath
}

// calculatePathScore calculates a score for path selection
func (ps *PacketScheduler) calculatePathScore(path *Path) float64 {
	// Factors to consider:
	// 1. RTT
	// 2. Available congestion window
	// 3. Loss rate
	// 4. Path priority
	// 5. Path stability

	if path.GetBytesInFlight() >= path.GetCongestionWindow() {
		return 0 // Path is blocked
	}

	// 处理 RTT 为 0 的情况
	var rttScore float64
	if path.metrics.SmoothedRTT == 0 {
		rttScore = 1.0 // 如果没有 RTT 样本，给一个默认值
	} else {
		rttScore = 1.0 / float64(path.metrics.SmoothedRTT)
	}

	cwndScore := float64(path.GetCongestionWindow() - path.GetBytesInFlight())
	lossScore := 1.0 - path.metrics.LossRate
	priorityScore := 1.0 / float64(path.Priority+1)

	// 计算稳定性得分
	stabilityScore := 1.0
	if path.metrics.RTTVar > 0 {
		stabilityScore = float64(path.metrics.SmoothedRTT) / float64(path.metrics.RTTVar)
	}

	// 调整权重
	// RTT: 20%
	// 拥塞窗口: 20%
	// 丢包率: 30%
	// 优先级: 10%
	// 稳定性: 20%
	return (rttScore * 0.2) + (cwndScore * 0.2) + (lossScore * 0.3) + (priorityScore * 0.1) + (stabilityScore * 0.2)
}

// HandleAck processes an ACK frame for a packet
func (ps *PacketScheduler) HandleAck(packetNumber PacketNumber, ackTime time.Time) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	packet, ok := ps.sentPackets[packetNumber]
	if !ok {
		return
	}

	path := ps.conn.paths[packet.PathID]
	if path == nil {
		return
	}

	path.ResetConsecutiveLosses()
	rtt := ackTime.Sub(packet.SendTime)
	path.metrics.UpdateRTT(rtt)

	path.AddBytesInFlight(-int64(packet.Length))
	if path.GetCongestionWindow() < path.slowStartThreshold {
		path.UpdateCongestionWindow(path.GetCongestionWindow() + packet.Length)
	} else {
		// 恢复之前的 CUBIC 拥塞控制逻辑，但添加溢出保护
		cwnd := float64(path.GetCongestionWindow())
		pktLen := float64(packet.Length)
		increase := uint64(pktLen * pktLen / cwnd)
		newWindow := safeAdd(path.GetCongestionWindow(), increase)
		path.UpdateCongestionWindow(newWindow)
	}

	path.metrics.AddBytesSent(packet.Length)
	delete(ps.sentPackets, packetNumber)

	ps.logPathMetrics(path, "PacketAcked", packet.Number)
}

// HandleLoss processes a packet loss event
func (ps *PacketScheduler) HandleLoss(packetNumber PacketNumber) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	packet, ok := ps.sentPackets[packetNumber]
	if !ok {
		log.Debug("忽略未知数据包的丢失事件: %d", packetNumber)
		return
	}

	path := ps.conn.paths[packet.PathID]
	if path == nil {
		log.Debug("忽略已删除路径上的丢失事件: %d", packet.PathID)
		return
	}

	path.metrics.UpdateLossRate(ps.calculateLossRate(path.ID))
	consecutiveLosses := path.IncrementConsecutiveLosses()

	if consecutiveLosses >= 5 || path.metrics.LossRate > 0.5 {
		ps.logPathMetrics(path, "PathFailure", packetNumber)

		if len(ps.conn.activePaths) > 1 {
			path.UpdateState(PathStateDraining)
			for i, activePathID := range ps.conn.activePaths {
				if activePathID == path.ID {
					ps.conn.activePaths = append(ps.conn.activePaths[:i], ps.conn.activePaths[i+1:]...)
					break
				}
			}
			ps.activateStandbyPath()
		}
	} else {
		ps.logPathMetrics(path, "PacketLost", packetNumber)
	}

	if packet.Retries < 3 {
		ps.scheduleRetransmission(packet)
	} else {
		log.Warn("数据包 %d 在 %d 次重试后放弃", packetNumber, packet.Retries)
	}
}

// activateStandbyPath attempts to activate a standby path
func (ps *PacketScheduler) activateStandbyPath() {
	for pathID := range ps.conn.paths {
		path := ps.conn.paths[pathID]
		if path.State == PathStateStandby && path.validationStatus {
			path.UpdateState(PathStateActive)
			ps.conn.activePaths = append(ps.conn.activePaths, path.ID)
			log.Info("激活备用路径 %d", path.ID)
			return
		}
	}
}

// calculateLossRate calculates the loss rate for a path
func (ps *PacketScheduler) calculateLossRate(pathID PathID) float64 {
	var lost, total float64

	for _, packet := range ps.lostPackets {
		if packet.PathID == pathID {
			lost++
		}
	}

	for _, packet := range ps.sentPackets {
		if packet.PathID == pathID {
			total++
		}
	}

	total += lost
	if total == 0 {
		return 0
	}

	return lost / total
}

// scheduleRetransmission schedules a packet for retransmission
func (ps *PacketScheduler) scheduleRetransmission(packet *Packet) {
	newPacket := &Packet{
		Type:    packet.Type,
		Frames:  packet.Frames,
		Length:  packet.Length,
		Retries: packet.Retries + 1,
	}

	// Track retransmission
	ps.retransmissions[packet.Number] = append(
		ps.retransmissions[packet.Number],
		PacketNumber(newPacket.Number),
	)

	// Schedule on a different path if possible
	if newPath := ps.selectRetransmissionPath(packet.PathID); newPath != nil {
		newPacket.PathID = newPath.ID
	}
}

// selectRetransmissionPath selects a path for retransmission
func (ps *PacketScheduler) selectRetransmissionPath(excludePathID PathID) *Path {
	var bestPath *Path
	var bestScore float64

	for _, pathID := range ps.conn.activePaths {
		if pathID == excludePathID {
			continue
		}

		path := ps.conn.paths[pathID]
		if !path.IsActive() || !path.IsValidated() {
			continue
		}

		score := ps.calculatePathScore(path)
		if score > bestScore {
			bestScore = score
			bestPath = path
		}
	}

	return bestPath
}

// Error types for packet scheduling
var (
	ErrNoAvailablePath = errors.New("no available path for transmission")
)

// CongestionController interface defines the methods required for congestion control
type CongestionController interface {
	OnPacketSent(bytes uint64)
	OnPacketAcked(packet *Packet, rtt time.Duration)
	OnPacketLost(packet *Packet)
	GetCongestionWindow() uint64
	InSlowStart() bool
}

// CubicConfig contains configuration for CUBIC congestion control
type CubicConfig struct {
	Beta            float64
	C               float64
	InitialWindow   uint64
	MinWindow       uint64
	MaxWindow       uint64
	FastConvergence bool
}

// DefaultCubicConfig returns the default CUBIC configuration
func DefaultCubicConfig() *CubicConfig {
	return &CubicConfig{
		Beta:            0.7,
		C:               0.4,
		InitialWindow:   14720,    // 10 full-sized packets
		MinWindow:       2940,     // 2 full-sized packets
		MaxWindow:       14720000, // ~10MB
		FastConvergence: true,
	}
}

// CubicState contains the state for CUBIC congestion control
type CubicState struct {
	config *CubicConfig

	// CUBIC state variables
	windowMax     uint64
	windowLastMax uint64
	k             float64
	originPoint   time.Time
	tcpFriendly   bool

	// Current state
	congestionWindow uint64
	ssthresh         uint64
	inSlowStart      bool

	// RTT tracking
	minRTT      time.Duration
	smoothedRTT time.Duration
	rttVar      time.Duration

	mu sync.Mutex
}

// NewCubicState creates a new CUBIC state
func NewCubicState(config *CubicConfig) *CubicState {
	if config == nil {
		config = DefaultCubicConfig()
	}

	return &CubicState{
		config:           config,
		congestionWindow: config.InitialWindow,
		ssthresh:         config.MaxWindow,
		inSlowStart:      true,
		tcpFriendly:      true,
	}
}

// OnPacketSent is called when a packet is sent
func (c *CubicState) OnPacketSent(bytes uint64) {
	// No window adjustment needed on packet send
}

// OnPacketAcked is called when a packet is acknowledged
func (c *CubicState) OnPacketAcked(packet *Packet, rtt time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Update RTT measurements
	c.updateRTT(rtt)

	if c.inSlowStart {
		// Slow start: increase window by one MSS per ACK
		c.congestionWindow = safeAdd(c.congestionWindow, maxDatagramSize)
		if c.congestionWindow >= c.ssthresh {
			c.inSlowStart = false
		}
	} else {
		// Congestion avoidance: CUBIC update
		c.cubicUpdate(packet.AckTime)
	}

	// Ensure window stays within bounds
	c.congestionWindow = min(c.congestionWindow, c.config.MaxWindow)
	c.congestionWindow = max(c.congestionWindow, c.config.MinWindow)
}

// OnPacketLost is called when a packet is lost
func (c *CubicState) OnPacketLost(packet *Packet) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.config.FastConvergence && c.congestionWindow < c.windowLastMax {
		c.windowLastMax = c.congestionWindow
		c.windowMax = uint64(float64(c.congestionWindow) * (2 - c.config.Beta) / 2)
	} else {
		c.windowLastMax = c.congestionWindow
		c.windowMax = c.congestionWindow
	}

	c.ssthresh = uint64(float64(c.congestionWindow) * c.config.Beta)
	c.congestionWindow = c.ssthresh
	c.inSlowStart = false
	c.k = c.calculateK()
	c.originPoint = time.Now()
}

// GetCongestionWindow returns the current congestion window
func (c *CubicState) GetCongestionWindow() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.congestionWindow
}

// InSlowStart returns whether the controller is in slow start
func (c *CubicState) InSlowStart() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inSlowStart
}

// cubicUpdate performs the CUBIC window update
func (c *CubicState) cubicUpdate(now time.Time) {
	t := now.Sub(c.originPoint).Seconds()
	target := c.calculateTarget(t)

	if c.tcpFriendly {
		// Calculate window size that TCP would have
		tcpWindow := c.calculateTCPWindow(t)
		if tcpWindow > target {
			target = tcpWindow
		}
	}

	c.congestionWindow = uint64(target)
}

// calculateTarget calculates the target window size
func (c *CubicState) calculateTarget(t float64) float64 {
	// CUBIC function: C(t) = C * (t - K)^3 + W_max
	diff := t - c.k
	return float64(c.windowMax) + c.config.C*diff*diff*diff
}

// calculateK calculates the time period
func (c *CubicState) calculateK() float64 {
	// K = cubic_root(W_max * (1 - beta) / C)
	return math.Cbrt(float64(c.windowMax) * (1 - c.config.Beta) / c.config.C)
}

// calculateTCPWindow calculates the window size that standard TCP would have
func (c *CubicState) calculateTCPWindow(t float64) float64 {
	// W_tcp(t) = W_max * beta + 3 * (1 - beta) / (1 + beta) * t / RTT
	rtt := c.smoothedRTT.Seconds()
	if rtt == 0 {
		rtt = 0.1 // Default RTT if no samples
	}

	return float64(c.windowMax)*c.config.Beta +
		3*(1-c.config.Beta)/(1+c.config.Beta)*t/rtt
}

// updateRTT updates the RTT measurements
func (c *CubicState) updateRTT(rtt time.Duration) {
	if c.minRTT == 0 || rtt < c.minRTT {
		c.minRTT = rtt
	}

	if c.smoothedRTT == 0 {
		c.smoothedRTT = rtt
		c.rttVar = rtt / 2
	} else {
		// EWMA updates
		diff := c.smoothedRTT - rtt
		if diff < 0 {
			diff = -diff
		}
		c.rttVar = (3*c.rttVar + diff) / 4
		c.smoothedRTT = (7*c.smoothedRTT + rtt) / 8
	}
}

// Constants for congestion control
const (
	maxDatagramSize = 1460 // Maximum size of a datagram (bytes)
)

// Helper functions
func min(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

func max(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

// safeAdd performs addition with overflow protection
func safeAdd(a, b uint64) uint64 {
	if a > math.MaxUint64-b {
		return math.MaxUint64
	}
	return a + b
}

// safeSub performs subtraction with underflow protection
func safeSub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}

// safeMul performs multiplication with overflow protection
func safeMul(a, b uint64) uint64 {
	if b != 0 && a > math.MaxUint64/b {
		return math.MaxUint64
	}
	return a * b
}

// safeDiv performs safe division for uint64 values
func safeDiv(a, b uint64) uint64 {
	if b == 0 {
		return 0
	}
	return a / b
}

// safeDurationAdd performs safe addition for time.Duration values
func safeDurationAdd(a, b time.Duration) time.Duration {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}
