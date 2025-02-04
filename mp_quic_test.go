package mpquic

import (
	"bytes"
	"fmt"
	"math"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/allnationconnect/websocket_tunnel/log"
)

// PathState 的补充定义
const (
	PathStateInvalid PathState = 4 // 添加Invalid状态
)

func TestPathManagement(t *testing.T) {
	config := &Config{
		MaxPaths:              4,
		PathValidationTimeout: 5 * time.Second,
		EnablePathMigration:   true,
		CongestionControl:     "cubic",
	}

	conn := NewConnection(config)

	// Test path addition
	local := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234}
	remote := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 5678}

	path, err := conn.AddPath(local, remote)
	if err != nil {
		t.Fatalf("Failed to add path: %v", err)
	}

	if path.ID != 0 {
		t.Errorf("Expected first path ID to be 0, got %d", path.ID)
	}

	if path.State != PathStateInitial {
		t.Errorf("Expected initial path state, got %v", path.State)
	}

	// Test path validation
	validationConfig := DefaultValidationConfig()
	err = path.StartValidation(validationConfig)
	if err != nil {
		t.Fatalf("Failed to start path validation: %v", err)
	}

	if path.validationStatus {
		t.Error("Path should not be validated immediately")
	}

	// Test path removal
	err = conn.RemovePath(path.ID)
	if err != nil {
		t.Fatalf("Failed to remove path: %v", err)
	}

	if len(conn.paths) != 0 {
		t.Errorf("Expected no paths after removal, got %d", len(conn.paths))
	}
}

func TestFrameEncoding(t *testing.T) {
	tests := []struct {
		name  string
		frame Frame
	}{
		{
			name: "PATH_CHALLENGE",
			frame: &PathChallengeFrame{
				Data: [8]byte{1, 2, 3, 4, 5, 6, 7, 8},
			},
		},
		{
			name: "PATH_RESPONSE",
			frame: &PathResponseFrame{
				Data: [8]byte{1, 2, 3, 4, 5, 6, 7, 8},
			},
		},
		{
			name: "PATH_STATUS",
			frame: &PathStatusFrame{
				PathID:     1,
				Status:     PathStateActive,
				SequenceNo: 42,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := tt.frame.Encode()
			if err != nil {
				t.Fatalf("Failed to encode frame: %v", err)
			}

			decoded, err := NewFrame(tt.frame.Type())
			if err != nil {
				t.Fatalf("Failed to create new frame: %v", err)
			}

			err = decoded.Decode(encoded)
			if err != nil {
				t.Fatalf("Failed to decode frame: %v", err)
			}

			// Compare encoded and decoded frames
			reencoded, err := decoded.Encode()
			if err != nil {
				t.Fatalf("Failed to re-encode frame: %v", err)
			}

			if !bytes.Equal(encoded, reencoded) {
				t.Errorf("Frame encoding/decoding mismatch:\nOriginal: %v\nRe-encoded: %v",
					encoded, reencoded)
			}
		})
	}
}

func TestCongestionControl(t *testing.T) {
	log.SetLogLevel("DEBUG")
	log.Info("开始 TestCongestionControl 测试")

	config := DefaultCubicConfig()
	cubic := NewCubicState(config)

	// 测试初始状态
	if !cubic.InSlowStart() {
		t.Error("期望从慢启动开始")
	}

	if w := cubic.GetCongestionWindow(); w != config.InitialWindow {
		t.Errorf("期望初始窗口 %d，实际为 %d", config.InitialWindow, w)
	}
	log.Debug("初始状态测试通过")

	// 测试慢启动行为
	packet := &Packet{
		Type:     PacketTypeOneRTT,
		Number:   1,
		Length:   1000,
		SendTime: time.Now(),
		AckTime:  time.Now().Add(100 * time.Millisecond),
	}

	initialWindow := cubic.GetCongestionWindow()
	cubic.OnPacketAcked(packet, 100*time.Millisecond)

	newWindow := cubic.GetCongestionWindow()
	if newWindow <= initialWindow {
		t.Error("慢启动期间窗口应该增加")
	}
	if newWindow > safeAdd(initialWindow, maxDatagramSize) {
		t.Error("慢启动期间窗口增加超过预期")
	}
	log.Debug("慢启动行为测试通过: %d -> %d", initialWindow, newWindow)

	// 测试丢包行为
	cubic.OnPacketLost(packet)

	if cubic.InSlowStart() {
		t.Error("丢包后应该退出慢启动")
	}

	lossWindow := cubic.GetCongestionWindow()
	if lossWindow >= initialWindow {
		t.Error("丢包后窗口应该减小")
	}
	log.Debug("丢包行为测试通过: %d -> %d", newWindow, lossWindow)

	// 测试拥塞避免
	for i := 0; i < 100; i++ {
		packet.Number = PacketNumber(i + 2)
		packet.AckTime = time.Now().Add(100 * time.Millisecond)
		cubic.OnPacketAcked(packet, 100*time.Millisecond)
	}

	finalWindow := cubic.GetCongestionWindow()
	if finalWindow < lossWindow {
		t.Error("拥塞避免期间窗口应该增加")
	}
	if finalWindow > config.MaxWindow {
		t.Error("窗口不应超过最大限制")
	}
	log.Debug("拥塞避免测试通过: %d -> %d", lossWindow, finalWindow)

	// 测试快速收敛
	if config.FastConvergence {
		lastMax := cubic.windowMax
		cubic.OnPacketLost(packet)
		if cubic.windowMax >= lastMax {
			t.Error("快速收敛应该降低最大窗口")
		}
		log.Debug("快速收敛测试通过: %d -> %d", lastMax, cubic.windowMax)
	}
}

func TestCIDManager(t *testing.T) {
	manager := NewCIDManager(4, 4, 8)

	// Test CID generation
	cid, err := manager.GenerateNewCID()
	if err != nil {
		t.Fatalf("Failed to generate CID: %v", err)
	}

	if len(cid) != 8 {
		t.Errorf("Expected CID length 8, got %d", len(cid))
	}

	// Test CID allocation
	cid, err = manager.AllocateLocalCID(1)
	if err != nil {
		t.Fatalf("Failed to allocate CID: %v", err)
	}

	pathID, exists := manager.GetPathByLocalCID(cid)
	if !exists {
		t.Fatal("CID not found after allocation")
	}

	if pathID != 1 {
		t.Errorf("Expected path ID 1, got %d", pathID)
	}

	// Test CID limits
	for i := 0; i < 4; i++ {
		manager.AllocateLocalCID(PathID(i))
	}

	_, err = manager.AllocateLocalCID(5)
	if err != ErrNoAvailableCIDs {
		t.Error("Should fail when exceeding max CIDs")
	}
}

// 添加辅助函数用于初始化测试路径
func initTestPath(t *testing.T, conn *Connection, local, remote *net.UDPAddr, rtt time.Duration) *Path {
	path, err := conn.AddPath(local, remote)
	if err != nil {
		t.Fatalf("添加路径失败: %v", err)
	}
	log.Debug("添加路径成功 - ID: %d, 本地: %v, 远程: %v",
		path.ID, local.String(), remote.String())

	path.mu.Lock()
	path.State = PathStateActive
	path.validationStatus = true
	path.metrics = PathMetrics{
		MinRTT:      rtt,
		SmoothedRTT: rtt,
		BytesSent:   0,
	}
	path.nextPacketNumber = 1
	path.congestionWindow = 32 * 1440 // 初始拥塞窗口 (32 个包)
	path.slowStartThreshold = 0
	path.bytesInFlight = 0
	path.mu.Unlock()

	conn.mu.Lock()
	conn.activePaths = append(conn.activePaths, path.ID)
	conn.paths[path.ID] = path
	conn.mu.Unlock()

	log.Debug("路径初始化完成 - ID: %d, RTT: %v, 拥塞窗口: %d, 已发送字节: %d",
		path.ID, rtt, path.congestionWindow, path.bytesInFlight)

	return path
}

func TestPacketScheduler(t *testing.T) {
	log.SetLogLevel("DEBUG")
	log.Info("开始 TestPacketScheduler 测试")

	config := &Config{
		MaxPaths:              4,
		PathValidationTimeout: 100 * time.Millisecond,
		EnablePathMigration:   true,
	}
	conn := NewConnection(config)
	scheduler := NewPacketScheduler(conn)
	log.Debug("创建了新的连接和调度器")

	// 初始化两条测试路径
	local1 := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234}
	remote1 := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 5678}
	path1 := initTestPath(t, conn, local1, remote1, 100*time.Millisecond)
	path1.Priority = 0 // 高优先级路径

	local2 := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1235}
	remote2 := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 5679}
	path2 := initTestPath(t, conn, local2, remote2, 150*time.Millisecond)
	path2.Priority = 1 // 低优先级路径

	log.Debug("初始化了两条路径，当前活跃路径数量: %d", len(conn.activePaths))

	// 测试路径选择
	frames := []Frame{
		&PathStatusFrame{
			PathID: 1,
			Status: PathStateActive,
		},
	}

	// 测试正常调度
	packet1, selectedPath, err := scheduler.SchedulePacket(frames)
	if err != nil {
		t.Fatalf("调度失败: %v", err)
	}
	if selectedPath.ID != path1.ID {
		t.Errorf("应该选择高优先级路径，但选择了路径 %d", selectedPath.ID)
	}
	log.Debug("正常调度测试通过 - 选择路径: %d", selectedPath.ID)

	// 测试拥塞窗口限制
	path1.UpdateCongestionWindow(1000) // 设置较小的拥塞窗口
	path1.AddBytesInFlight(900)        // 添加较大的在途字节数

	packet2, selectedPath, err := scheduler.SchedulePacket(frames)
	if err != nil {
		t.Fatalf("调度失败: %v", err)
	}
	if selectedPath.ID != path2.ID {
		t.Errorf("拥塞时应该选择备用路径，但选择了路径 %d", selectedPath.ID)
	}
	log.Debug("拥塞控制测试通过 - 切换到路径: %d", selectedPath.ID)

	// 测试丢包处理
	scheduler.HandleLoss(packet1.Number)
	path1Losses := path1.GetConsecutiveLosses()
	if path1Losses == 0 {
		t.Error("丢包计数器应该增加")
	}
	log.Debug("丢包处理测试通过 - 连续丢包数: %d", path1Losses)

	// 测试确认处理
	initialWindow := path2.GetCongestionWindow()
	scheduler.HandleAck(packet2.Number, time.Now())
	newWindow := path2.GetCongestionWindow()
	if newWindow <= initialWindow {
		t.Error("确认后拥塞窗口应该增加")
	}
	log.Debug("确认处理测试通过 - 拥塞窗口: %d -> %d", initialWindow, newWindow)

	// 测试路径失败处理
	for i := 0; i < 5; i++ {
		scheduler.HandleLoss(PacketNumber(i + 100))
	}
	if path1.State != PathStateDraining {
		t.Error("多次连续丢包后路径应该进入draining状态")
	}
	log.Debug("路径失败处理测试通过 - 路径状态: %v", path1.State)
}

// 添加辅助函数用于路径状态检查
func checkPathState(t *testing.T, path *Path, expectedState PathState, expectedValidation bool) {
	path.mu.Lock()
	defer path.mu.Unlock()

	if path.State != expectedState {
		t.Errorf("期望路径状态为 %v，实际得到 %v", expectedState, path.State)
	}
	if path.validationStatus != expectedValidation {
		t.Errorf("期望验证状态为 %v，实际得到 %v", expectedValidation, path.validationStatus)
	}
	log.Debug("路径状态检查 - ID: %d, 状态: %v, 验证状态: %v, RTT: %v",
		path.ID, path.State, path.validationStatus, path.metrics.SmoothedRTT)
}

// 添加辅助函数用于路径验证，带超时控制
func validatePathWithTimeout(t *testing.T, path *Path, config ValidationConfig, timeout time.Duration) error {
	log.Debug("开始路径验证 - ID: %d, 超时: %v, 最大重试: %d",
		path.ID, config.Timeout, config.MaxRetries)

	// 创建一个通道用于同步验证结果
	done := make(chan error, 1)
	validationStartTime := time.Now()

	go func() {
		defer func() {
			log.Debug("验证 goroutine 结束 - ID: %d, 耗时: %v",
				path.ID, time.Since(validationStartTime))
		}()

		// 在 goroutine 中执行验证
		path.mu.Lock()
		log.Debug("获取到路径锁 - ID: %d", path.ID)

		// 保存当前验证状态
		if path.validationTimer != nil {
			path.validationTimer.Stop()
			path.validationTimer = nil
			log.Debug("停止现有验证定时器 - ID: %d", path.ID)
		}

		// 重置状态
		path.validationStatus = false
		path.State = PathStateInitial
		log.Debug("重置路径状态 - ID: %d, 状态: %v, 验证: %v",
			path.ID, path.State, path.validationStatus)

		// 生成验证数据
		path.challengeData = make([]byte, config.ChallengeSize)
		if _, err := rand.Read(path.challengeData); err != nil {
			path.mu.Unlock()
			log.Error("生成验证数据失败 - ID: %d, 错误: %v", path.ID, err)
			done <- fmt.Errorf("生成验证数据失败: %v", err)
			return
		}

		// 复制验证数据
		challengeData := make([]byte, len(path.challengeData))
		copy(challengeData, path.challengeData)
		log.Debug("生成验证数据 - ID: %d, 大小: %d", path.ID, len(challengeData))

		// 设置定时器
		validationTimer := time.NewTimer(config.Timeout)
		path.validationTimer = validationTimer
		log.Debug("设置验证定时器 - ID: %d, 超时: %v", path.ID, config.Timeout)

		path.mu.Unlock()
		log.Debug("释放路径锁 - ID: %d", path.ID)

		// 等待一小段时间后尝试验证
		time.Sleep(10 * time.Millisecond)
		log.Debug("开始处理验证响应 - ID: %d", path.ID)

		result := path.HandlePathResponse(challengeData)
		log.Debug("验证响应处理完成 - ID: %d, 结果: %v", path.ID, result)

		if result != PathValidationSuccess {
			done <- fmt.Errorf("验证失败，结果: %v", result)
			return
		}

		// 验证最终状态
		path.mu.Lock()
		finalState := path.State
		finalValidation := path.validationStatus
		path.mu.Unlock()
		log.Debug("最终状态检查 - ID: %d, 状态: %v, 验证: %v",
			path.ID, finalState, finalValidation)

		if !finalValidation || finalState != PathStateActive {
			done <- fmt.Errorf("验证后状态异常：验证=%v, 状态=%v",
				finalValidation, finalState)
			return
		}

		done <- nil
	}()

	// 等待验证完成或总超时
	log.Debug("等待验证完成 - ID: %d, 最大等待时间: %v", path.ID, timeout)
	select {
	case err := <-done:
		if err != nil {
			log.Error("验证失败 - ID: %d, 错误: %v", path.ID, err)
		} else {
			log.Debug("验证成功完成 - ID: %d, 耗时: %v",
				path.ID, time.Since(validationStartTime))
		}
		return err
	case <-time.After(timeout):
		log.Error("验证总超时 - ID: %d, 超时时间: %v", path.ID, timeout)
		// 在超时时清理状态
		path.mu.Lock()
		if path.validationTimer != nil {
			path.validationTimer.Stop()
			path.validationTimer = nil
		}
		path.UpdateState(PathStateDraining)
		path.mu.Unlock()
		return fmt.Errorf("验证总超时（%v）", timeout)
	}
}

func TestPathLifecycle(t *testing.T) {
	log.SetLogLevel("DEBUG")
	log.Info("开始 TestPathLifecycle 测试")

	// 注意：整个测试应该在 500ms 内完成，如果超过这个时间就可能是卡住了
	testTimeout := 500 * time.Millisecond
	testStart := time.Now()
	defer func() {
		log.Info("TestPathLifecycle 耗时: %v", time.Since(testStart))
	}()

	config := &Config{
		MaxPaths:              4,
		PathValidationTimeout: 100 * time.Millisecond,
		EnablePathMigration:   true,
		CongestionControl:     "cubic",
	}
	log.Debug("创建配置: MaxPaths=%d, Timeout=%v", config.MaxPaths, config.PathValidationTimeout)

	conn := NewConnection(config)
	log.Debug("创建了新的连接")

	// Test path addition
	local := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234}
	remote := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 5678}

	path, err := conn.AddPath(local, remote)
	if err != nil {
		t.Fatalf("添加路径失败: %v", err)
	}
	log.Debug("添加路径成功，路径ID: %d", path.ID)

	// 检查初始状态
	checkPathState(t, path, PathStateInitial, false)

	// 执行路径验证（设置较短的超时时间）
	validationConfig := DefaultValidationConfig()
	validationConfig.Timeout = 100 * time.Millisecond
	validationConfig.MaxRetries = 1

	err = validatePathWithTimeout(t, path, validationConfig, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("路径验证失败: %v", err)
	}

	// 检查验证后状态
	checkPathState(t, path, PathStateActive, true)

	// 更新和检查指标
	updatePathMetrics(t, path, 100*time.Millisecond)

	// 移除路径
	err = conn.RemovePath(path.ID)
	if err != nil {
		t.Fatalf("移除路径失败: %v", err)
	}
	log.Debug("移除路径成功，剩余路径数: %d", len(conn.paths))

	// 确保测试在预期时间内完成
	if elapsed := time.Since(testStart); elapsed > testTimeout {
		t.Errorf("测试执行时间过长: %v > %v", elapsed, testTimeout)
	}

	log.Info("TestPathLifecycle 测试完成")
}

// TestPacketSchedulingAndRetransmission tests packet scheduling and retransmission
func TestPacketSchedulingAndRetransmission(t *testing.T) {
	log.SetLogLevel("DEBUG")
	log.Info("开始 TestPacketSchedulingAndRetransmission 测试")

	conn := NewConnection(nil)
	scheduler := NewPacketScheduler(conn)

	// Add two paths
	local1 := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234}
	remote1 := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 5678}
	path1, _ := conn.AddPath(local1, remote1)

	// 初始化第一条路径
	path1.mu.Lock()
	path1.State = PathStateActive
	path1.validationStatus = true
	path1.metrics.SmoothedRTT = 100 * time.Millisecond
	path1.congestionWindow = 32 * 1440 // 初始拥塞窗口
	path1.bytesInFlight = 0
	path1.nextPacketNumber = 1
	path1.mu.Unlock()

	conn.mu.Lock()
	conn.activePaths = append(conn.activePaths, path1.ID)
	conn.mu.Unlock()
	log.Debug("添加第一条路径 - ID: %d, 状态: Active, 验证: true, RTT: %v",
		path1.ID, path1.metrics.SmoothedRTT)

	local2 := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1235}
	remote2 := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 5679}
	path2, _ := conn.AddPath(local2, remote2)

	// 初始化第二条路径
	path2.mu.Lock()
	path2.State = PathStateActive
	path2.validationStatus = true
	path2.metrics.SmoothedRTT = 150 * time.Millisecond
	path2.congestionWindow = 32 * 1440 // 初始拥塞窗口
	path2.bytesInFlight = 0
	path2.nextPacketNumber = 1
	path2.mu.Unlock()

	conn.mu.Lock()
	conn.activePaths = append(conn.activePaths, path2.ID)
	conn.mu.Unlock()
	log.Debug("添加第二条路径 - ID: %d, 状态: Active, 验证: true, RTT: %v",
		path2.ID, path2.metrics.SmoothedRTT)

	// 检查路径状态
	for _, pathID := range conn.activePaths {
		if path, ok := conn.paths[pathID]; ok {
			log.Debug("路径状态 - ID: %d, 状态: %v, 验证: %v, RTT: %v, 拥塞窗口: %v",
				pathID, path.State, path.validationStatus, path.metrics.SmoothedRTT,
				path.congestionWindow)
		}
	}

	// Create test frames
	frames := []Frame{
		&PathStatusFrame{
			PathID: 1,
			Status: PathStateActive,
		},
	}
	log.Debug("创建测试帧")

	// Test initial packet scheduling
	packet1, path, err := scheduler.SchedulePacket(frames)
	if err != nil {
		t.Fatalf("数据包调度失败: %v", err)
	}

	// Verify packet was scheduled
	if packet1 == nil || path == nil {
		t.Fatal("调度返回的数据包或路径为空")
	}
	log.Debug("第一次调度成功 - 包号: %d, 路径ID: %d", packet1.Number, path.ID)

	// Simulate packet loss
	scheduler.HandleLoss(packet1.Number)
	log.Debug("模拟丢包 - 包号: %d", packet1.Number)

	// Verify packet was marked as lost
	if _, exists := scheduler.lostPackets[packet1.Number]; !exists {
		t.Error("数据包应该被标记为丢失")
	}

	// Verify retransmission was scheduled
	if len(scheduler.retransmissions[packet1.Number]) == 0 {
		t.Error("应该安排重传")
	}
	log.Debug("验证重传安排")

	// Test ACK handling
	packet2, path, err := scheduler.SchedulePacket(frames)
	if err != nil {
		t.Fatalf("第二次调度失败: %v", err)
	}
	log.Debug("第二次调度成功 - 包号: %d, 路径ID: %d", packet2.Number, path.ID)

	ackTime := time.Now()
	scheduler.HandleAck(packet2.Number, ackTime)
	log.Debug("处理确认 - 包号: %d, 时间: %v", packet2.Number, ackTime)

	// Verify ACK processing
	if _, exists := scheduler.sentPackets[packet2.Number]; exists {
		t.Error("确认后数据包应该被移除")
	}

	log.Info("TestPacketSchedulingAndRetransmission 测试完成")
}

// TestCongestionControlBehavior tests detailed congestion control behavior
func TestCongestionControlBehavior(t *testing.T) {
	log.SetLogLevel("DEBUG")

	config := DefaultCubicConfig()
	cubic := NewCubicState(config)

	// Test initial state
	if !cubic.InSlowStart() {
		t.Error("Expected to start in slow start")
	}
	if w := cubic.GetCongestionWindow(); w != config.InitialWindow {
		t.Errorf("Expected initial window %d, got %d", config.InitialWindow, w)
	}

	// Test slow start phase
	initialWindow := cubic.GetCongestionWindow()
	for i := 0; i < 10; i++ {
		packet := &Packet{
			Type:     PacketTypeOneRTT,
			Number:   PacketNumber(i),
			Length:   1000,
			SendTime: time.Now(),
			AckTime:  time.Now().Add(50 * time.Millisecond),
		}
		cubic.OnPacketAcked(packet, 50*time.Millisecond)
	}

	if w := cubic.GetCongestionWindow(); w <= initialWindow {
		t.Error("Window should increase significantly during slow start")
	}

	// Test congestion avoidance phase
	cubic.OnPacketLost(&Packet{Number: 11})
	if cubic.InSlowStart() {
		t.Error("Should exit slow start after loss")
	}

	preWindow := cubic.GetCongestionWindow()
	for i := 0; i < 100; i++ {
		packet := &Packet{
			Type:     PacketTypeOneRTT,
			Number:   PacketNumber(i + 12),
			Length:   1000,
			SendTime: time.Now(),
			AckTime:  time.Now().Add(50 * time.Millisecond),
		}
		cubic.OnPacketAcked(packet, 50*time.Millisecond)
	}

	if w := cubic.GetCongestionWindow(); w <= preWindow {
		t.Error("Window should increase during congestion avoidance")
	}
}

// BenchmarkPacketScheduling benchmarks the packet scheduling performance
func BenchmarkPacketScheduling(b *testing.B) {
	conn := NewConnection(nil)
	scheduler := NewPacketScheduler(conn)

	// Add test paths
	local1 := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234}
	remote1 := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 5678}
	path1, _ := conn.AddPath(local1, remote1)
	path1.UpdateState(PathStateActive)
	path1.validationStatus = true

	local2 := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1235}
	remote2 := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 5679}
	path2, _ := conn.AddPath(local2, remote2)
	path2.UpdateState(PathStateActive)
	path2.validationStatus = true

	frames := []Frame{
		&PathStatusFrame{
			PathID: 1,
			Status: PathStateActive,
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		packet, _, err := scheduler.SchedulePacket(frames)
		if err != nil {
			b.Fatalf("Failed to schedule packet: %v", err)
		}
		scheduler.HandleAck(packet.Number, time.Now())
	}
}

// BenchmarkCongestionControl benchmarks the congestion control performance
func BenchmarkCongestionControl(b *testing.B) {
	cubic := NewCubicState(DefaultCubicConfig())
	packet := &Packet{
		Type:     PacketTypeOneRTT,
		Length:   1000,
		SendTime: time.Now(),
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		packet.Number = PacketNumber(i)
		packet.AckTime = time.Now()
		cubic.OnPacketAcked(packet, 50*time.Millisecond)
		if i%100 == 99 {
			cubic.OnPacketLost(packet)
		}
	}
}

// 添加辅助函数用于路径指标更新
func updatePathMetrics(t *testing.T, path *Path, rtt time.Duration) {
	path.mu.Lock()
	oldRTT := path.metrics.SmoothedRTT
	path.mu.Unlock()

	path.UpdateMetrics(rtt)

	path.mu.Lock()
	newRTT := path.metrics.SmoothedRTT
	path.mu.Unlock()

	log.Debug("更新路径指标 - ID: %d, 旧RTT: %v, 新RTT: %v",
		path.ID, oldRTT, newRTT)
}

// TestComplexPathScenarios 测试复杂的多路径场景
func TestComplexPathScenarios(t *testing.T) {
	log.SetLogLevel("DEBUG")
	log.Info("开始 TestComplexPathScenarios 测试")

	config := &Config{
		MaxPaths:              8, // 支持更多路径
		PathValidationTimeout: 100 * time.Millisecond,
		EnablePathMigration:   true,
	}
	conn := NewConnection(config)
	scheduler := NewPacketScheduler(conn)

	// 1. 创建具有不同特征的多条路径
	paths := make([]*Path, 0)

	// 主路径 - 低延迟，高带宽
	local1 := &net.UDPAddr{IP: net.ParseIP("192.168.1.1"), Port: 1234}
	remote1 := &net.UDPAddr{IP: net.ParseIP("192.168.1.2"), Port: 5678}
	path1, _ := conn.AddPath(local1, remote1)
	path1.mu.Lock()
	path1.Priority = 0 // 最高优先级
	path1.metrics.SmoothedRTT = 20 * time.Millisecond
	path1.congestionWindow = 64 * 1440 // 较大的拥塞窗口
	path1.mu.Unlock()
	paths = append(paths, path1)
	log.Debug("添加主路径 - ID: %d, RTT: 20ms, 优先级: 0", path1.ID)

	// 备用路径 - 中等延迟，中等带宽
	local2 := &net.UDPAddr{IP: net.ParseIP("192.168.2.1"), Port: 1235}
	remote2 := &net.UDPAddr{IP: net.ParseIP("192.168.2.2"), Port: 5679}
	path2, _ := conn.AddPath(local2, remote2)
	path2.mu.Lock()
	path2.Priority = 1
	path2.metrics.SmoothedRTT = 50 * time.Millisecond
	path2.congestionWindow = 32 * 1440
	path2.mu.Unlock()
	paths = append(paths, path2)
	log.Debug("添加备用路径 - ID: %d, RTT: 50ms, 优先级: 1", path2.ID)

	// 非对称路径 - 上行带宽低，下行带宽高
	local3 := &net.UDPAddr{IP: net.ParseIP("192.168.3.1"), Port: 1236}
	remote3 := &net.UDPAddr{IP: net.ParseIP("192.168.3.2"), Port: 5680}
	path3, _ := conn.AddPath(local3, remote3)
	path3.mu.Lock()
	path3.Priority = 2
	path3.metrics.SmoothedRTT = 100 * time.Millisecond
	path3.congestionWindow = 16 * 1440 // 较小的上行带宽
	path3.mu.Unlock()
	paths = append(paths, path3)
	log.Debug("添加非对称路径 - ID: %d, RTT: 100ms, 优先级: 2", path3.ID)

	// 不稳定路径 - 高延迟，高丢包率
	local4 := &net.UDPAddr{IP: net.ParseIP("192.168.4.1"), Port: 1237}
	remote4 := &net.UDPAddr{IP: net.ParseIP("192.168.4.2"), Port: 5681}
	path4, _ := conn.AddPath(local4, remote4)
	path4.mu.Lock()
	path4.Priority = 3
	path4.metrics.SmoothedRTT = 200 * time.Millisecond
	path4.metrics.LossRate = 0.1 // 10% 丢包率
	path4.congestionWindow = 8 * 1440
	path4.mu.Unlock()
	paths = append(paths, path4)
	log.Debug("添加不稳定路径 - ID: %d, RTT: 200ms, 优先级: 3, 丢包率: 10%%", path4.ID)

	// 2. 并发路径验证
	log.Info("开始并发路径验证")
	validationConfig := ValidationConfig{
		Timeout:       100 * time.Millisecond,
		MaxRetries:    2,
		ChallengeSize: 8,
	}

	var wg sync.WaitGroup
	validationResults := make(map[PathID]error)
	var validationMutex sync.Mutex

	for _, p := range paths {
		wg.Add(1)
		go func(path *Path) {
			defer wg.Done()
			err := validatePathWithTimeout(t, path, validationConfig, 200*time.Millisecond)
			validationMutex.Lock()
			validationResults[path.ID] = err
			validationMutex.Unlock()
		}(p)
	}

	wg.Wait()
	log.Debug("所有路径验证完成，检查结果")

	// 检查验证结果
	for pathID, err := range validationResults {
		if err != nil {
			t.Errorf("路径 %d 验证失败: %v", pathID, err)
		} else {
			log.Debug("路径 %d 验证成功", pathID)
		}
	}

	// 3. 测试路径状态转换
	log.Info("开始测试路径状态转换")

	// 将备用路径转换为 Standby
	path2.UpdateState(PathStateStandby)
	if path2.State != PathStateStandby {
		t.Errorf("路径 %d 状态转换失败，期望 Standby，实际 %v", path2.ID, path2.State)
	}
	log.Debug("路径 %d 转换为 Standby 状态", path2.ID)

	// 将不稳定路径转换为 Draining
	path4.UpdateState(PathStateDraining)
	if path4.State != PathStateDraining {
		t.Errorf("路径 %d 状态转换失败，期望 Draining，实际 %v", path4.ID, path4.State)
	}
	log.Debug("路径 %d 转换为 Draining 状态", path4.ID)

	// 4. 测试路径调度
	log.Info("开始测试路径调度")

	// 确保主路径处于Active状态
	path1.mu.Lock()
	path1.State = PathStateActive
	path1.validationStatus = true
	path1.mu.Unlock()
	conn.activePaths = append(conn.activePaths, path1.ID)

	frames := []Frame{
		&PathStatusFrame{
			PathID: 1,
			Status: PathStateActive,
		},
	}

	// 记录每个路径被选择的次数
	pathSelectionCount := make(map[PathID]int)

	// 进行多次调度，观察路径选择情况
	for i := 0; i < 100; i++ {
		packet, path, err := scheduler.SchedulePacket(frames)
		if err != nil {
			t.Fatalf("数据包调度失败: %v", err)
		}
		pathSelectionCount[path.ID]++

		// 模拟一些包的确认和丢失
		if i%10 == 0 {
			scheduler.HandleLoss(packet.Number)
			log.Debug("模拟丢包 - 包号: %d, 路径: %d", packet.Number, path.ID)
		} else {
			scheduler.HandleAck(packet.Number, time.Now())
			log.Debug("模拟确认 - 包号: %d, 路径: %d", packet.Number, path.ID)
		}
	}

	// 检查路径选择分布
	log.Debug("路径选择统计：")
	for pathID, count := range pathSelectionCount {
		log.Debug("路径 %d 被选择 %d 次", pathID, count)
	}

	// 5. 测试路径故障转移
	log.Info("开始测试路径故障转移")

	// 将备用路径转换为 Active 状态
	path2.mu.Lock()
	path2.State = PathStateActive
	path2.validationStatus = true
	path2.mu.Unlock()
	conn.activePaths = append(conn.activePaths, path2.ID)

	// 模拟主路径故障
	log.Debug("模拟主路径故障，转换为 Draining 状态")
	path1.UpdateState(PathStateDraining)

	// 验证调度器是否会选择备用路径
	_, path, err := scheduler.SchedulePacket(frames)
	if err != nil {
		t.Fatalf("故障转移后调度失败: %v", err)
	}
	if path.ID == path1.ID {
		t.Error("故障转移失败，调度器仍在使用故障路径")
	}
	log.Debug("故障转移成功，数据包被调度到路径 %d", path.ID)

	// 6. 清理资源
	log.Info("开始清理资源")
	for _, p := range paths {
		err := conn.RemovePath(p.ID)
		if err != nil {
			t.Errorf("移除路径 %d 失败: %v", p.ID, err)
		}
		log.Debug("移除路径 %d", p.ID)
	}

	log.Info("TestComplexPathScenarios 测试完成")
}

type pathMetricsData struct {
	selectionCount int
	lossCount      int
	totalRTT       time.Duration
	rttCount       int
	lastRTT        time.Duration
	maxRTT         time.Duration
	minRTT         time.Duration
	totalBytes     uint64
	mu             sync.Mutex
}

func TestMassivePathScenarios(t *testing.T) {
	log.SetLogLevel("DEBUG")
	log.Info("开始大规模多路径压力测试")

	testDuration := 5*time.Minute + 10*time.Second // 5分钟测试时间加10秒缓冲
	testStart := time.Now()
	lastStatsTime := testStart

	done := make(chan bool)
	go func() {
		defer close(done)

		config := &Config{
			MaxPaths:              64,                    // 支持最多64条路径
			PathValidationTimeout: 50 * time.Millisecond, // 降低验证超时时间
			EnablePathMigration:   true,
		}
		conn := NewConnection(config)
		scheduler := NewPacketScheduler(conn)

		// 定义路径组特征
		pathGroups := []struct {
			count    int
			baseRTT  time.Duration
			baseBW   int
			basePort int
			priority int
			lossRate float64
		}{
			{10, 20 * time.Millisecond, 100 * 1440, 1234, 0, 0.01}, // 高性能路径组
			{10, 50 * time.Millisecond, 50 * 1440, 2234, 1, 0.05},  // 中等性能路径组
			{10, 100 * time.Millisecond, 30 * 1440, 3234, 2, 0.08}, // 低性能路径组
		}

		paths := make([]*Path, 0)
		var pathMetrics sync.Map

		// 创建路径
		log.Debug("开始创建初始路径...")
		var nextPathID int = 0
		var pathMutex sync.Mutex

		// 修改createPath函数
		createPath := func(groupIdx int) *Path {
			group := pathGroups[groupIdx]
			pathMutex.Lock()
			currentPathID := nextPathID
			nextPathID++
			pathMutex.Unlock()

			local := &net.UDPAddr{
				IP:   net.ParseIP(fmt.Sprintf("192.168.%d.1", currentPathID+1)),
				Port: group.basePort + currentPathID,
			}
			remote := &net.UDPAddr{
				IP:   net.ParseIP(fmt.Sprintf("192.168.%d.2", currentPathID+1)),
				Port: group.basePort + 1000 + currentPathID,
			}

			path, _ := conn.AddPath(local, remote)
			path.mu.Lock()
			path.Priority = uint8(group.priority)
			path.metrics.SmoothedRTT = group.baseRTT
			path.metrics.LossRate = group.lossRate
			path.congestionWindow = uint64(group.baseBW * 2)
			path.State = PathStateActive
			path.validationStatus = true
			path.mu.Unlock()

			pathMetrics.Store(path.ID, &pathMetricsData{
				minRTT: time.Hour, // 初始化为一个较大的值
			})

			log.Debug("创建新路径 - ID: %d, 组: %d, RTT: %v, 丢包率: %.2f%%",
				path.ID, groupIdx, group.baseRTT, group.lossRate*100)

			return path
		}

		// 初始化路径
		for groupIdx, group := range pathGroups {
			for i := 0; i < group.count; i++ {
				path := createPath(groupIdx)
				paths = append(paths, path)
				conn.activePaths = append(conn.activePaths, path.ID)
			}
		}

		frames := []Frame{
			&PathStatusFrame{
				PathID: 1,
				Status: PathStateActive,
			},
		}

		testStart := time.Now()
		packetNumber := uint64(0)

		// 创建停止通道
		stopChan := make(chan struct{})
		var wg sync.WaitGroup

		// 启动网络条件干扰器
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()

			for {
				select {
				case <-stopChan:
					return
				case <-ticker.C:
					pathMutex.Lock()
					if len(paths) == 0 {
						pathMutex.Unlock()
						continue
					}
					// 随机选择一条路径进行网络条件调整
					randomPath := paths[rand.Intn(len(paths))]
					pathMutex.Unlock()

					randomPath.mu.Lock()
					// RTT波动：±50ms，且有10%概率发生大幅波动(+100ms)
					rttDelta := time.Duration(rand.Int63n(100)-50) * time.Millisecond
					if rand.Float64() < 0.1 {
						rttDelta += 100 * time.Millisecond
					}
					newRTT := randomPath.metrics.SmoothedRTT + rttDelta
					if newRTT < time.Millisecond {
						newRTT = time.Millisecond
					}
					randomPath.metrics.SmoothedRTT = newRTT

					// 丢包率波动：±2%，且有5%概率发生突发丢包(+10%)
					lossDelta := (rand.Float64() - 0.5) * 0.04
					if rand.Float64() < 0.05 {
						lossDelta += 0.1
					}
					newLossRate := randomPath.metrics.LossRate + lossDelta
					if newLossRate < 0.0 {
						newLossRate = 0.0
					} else if newLossRate > 1.0 {
						newLossRate = 1.0
					}
					randomPath.metrics.LossRate = newLossRate
					randomPath.mu.Unlock()
				}
			}
		}()

		// 启动路径管理器
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()

			for {
				select {
				case <-stopChan:
					return
				case <-ticker.C:
					pathMutex.Lock()
					currentPaths := make([]*Path, len(paths))
					copy(currentPaths, paths)
					pathMutex.Unlock()

					// 随机关闭路径
					if len(currentPaths) > 5 && rand.Float64() < 0.2 { // 20%概率关闭一个路径
						pathToClose := currentPaths[rand.Intn(len(currentPaths))]
						log.Debug("准备关闭路径 - ID: %d", pathToClose.ID)

						// 更新路径状态为draining
						pathToClose.UpdateState(PathStateDraining)

						// 从活动路径列表中移除
						pathMutex.Lock()
						for i, p := range paths {
							if p.ID == pathToClose.ID {
								paths = append(paths[:i], paths[i+1:]...)
								break
							}
						}
						pathMutex.Unlock()

						log.Debug("成功关闭路径 - ID: %d", pathToClose.ID)
					}

					// 随机添加新路径
					if len(currentPaths) < 40 && rand.Float64() < 0.3 { // 30%概率添加新路径
						groupIdx := rand.Intn(len(pathGroups))
						newPath := createPath(groupIdx)
						pathMutex.Lock()
						paths = append(paths, newPath)
						conn.activePaths = append(conn.activePaths, newPath.ID)
						pathMutex.Unlock()
					}

					// 随机触发主路径故障
					if rand.Float64() < 0.1 { // 10%概率触发主路径故障
						pathMutex.Lock()
						if len(paths) > 0 {
							// 找出使用最多的路径
							var maxPath *Path
							var maxSelections int
							for _, p := range paths {
								if val, ok := pathMetrics.Load(p.ID); ok {
									metrics := val.(*pathMetricsData)
									metrics.mu.Lock()
									if metrics.selectionCount > maxSelections {
										maxSelections = metrics.selectionCount
										maxPath = p
									}
									metrics.mu.Unlock()
								}
							}

							if maxPath != nil {
								log.Debug("触发主路径故障 - ID: %d", maxPath.ID)
								maxPath.mu.Lock()
								maxPath.metrics.LossRate = 0.8   // 设置高丢包率
								maxPath.metrics.SmoothedRTT *= 3 // 显著增加RTT
								maxPath.mu.Unlock()
							}
						}
						pathMutex.Unlock()
					}
				}
			}
		}()

		// 主测试循环
		for time.Since(testStart) < testDuration {
			elapsed := time.Since(testStart)
			pathMutex.Lock()
			if len(paths) == 0 {
				pathMutex.Unlock()
				time.Sleep(100 * time.Millisecond)
				continue
			}
			pathMutex.Unlock()

			packet, path, err := scheduler.SchedulePacket(frames)
			if err != nil {
				continue
			}

			packetNumber++
			if val, ok := pathMetrics.Load(path.ID); ok {
				if metrics, ok := val.(*pathMetricsData); ok {
					metrics.mu.Lock()
					metrics.selectionCount++
					metrics.totalBytes = safeAdd(metrics.totalBytes, 1440)
					metrics.mu.Unlock()
				}
			}

			// 模拟数据包传输
			path.mu.Lock()
			currentRTT := path.metrics.SmoothedRTT
			lossRate := path.metrics.LossRate
			path.mu.Unlock()

			if rand.Float64() < lossRate {
				scheduler.HandleLoss(packet.Number)
				if val, ok := pathMetrics.Load(path.ID); ok {
					if metrics, ok := val.(*pathMetricsData); ok {
						metrics.mu.Lock()
						metrics.lossCount++
						metrics.mu.Unlock()
					}
				}
			} else {
				ackTime := time.Now().Add(currentRTT)
				scheduler.HandleAck(packet.Number, ackTime)

				if val, ok := pathMetrics.Load(path.ID); ok {
					if metrics, ok := val.(*pathMetricsData); ok {
						metrics.mu.Lock()
						metrics.totalRTT = safeDurationAdd(metrics.totalRTT, currentRTT)
						metrics.rttCount++
						metrics.lastRTT = currentRTT
						if currentRTT > metrics.maxRTT {
							metrics.maxRTT = currentRTT
						}
						if metrics.minRTT == 0 || currentRTT < metrics.minRTT {
							metrics.minRTT = currentRTT
						}
						metrics.mu.Unlock()
					}
				}
			}

			// 每5秒输出一次统计信息
			if time.Since(lastStatsTime) >= 5*time.Second {
				lastStatsTime = time.Now()

				log.Info("===== 测试进度报告 [%v/%v] =====", elapsed.Round(time.Second), testDuration)

				// 计算总吞吐量
				var totalBytes uint64
				pathMetrics.Range(func(key, value interface{}) bool {
					if m, ok := value.(*pathMetricsData); ok {
						m.mu.Lock()
						totalBytes = safeAdd(totalBytes, m.totalBytes)
						m.mu.Unlock()
					}
					return true
				})
				throughput := float64(totalBytes) / elapsed.Seconds() / 1024 / 1024 // MB/s
				log.Info("总吞吐量: %.2f MB/s, 总包数: %d", throughput, packetNumber)

				// 输出每个活跃路径的统计信息
				pathMutex.Lock()
				activePaths := make([]*Path, len(paths))
				copy(activePaths, paths)
				pathMutex.Unlock()

				log.Info("活跃路径数量: %d", len(activePaths))
				for _, p := range activePaths {
					if val, ok := pathMetrics.Load(p.ID); ok {
						if metrics, ok := val.(*pathMetricsData); ok {
							metrics.mu.Lock()
							if metrics.selectionCount > 0 {
								// 计算平均RTT
								avgRTT := time.Duration(safeDiv(uint64(metrics.totalRTT), uint64(metrics.rttCount)))
								lossRate := float64(metrics.lossCount) / float64(metrics.selectionCount)
								pathThroughput := float64(metrics.totalBytes) / elapsed.Seconds() / 1024 / 1024
								log.Info("路径 %d - 选择次数: %d, 平均RTT: %v, 最大RTT: %v, 最小RTT: %v, 丢包率: %.2f%%, 吞吐量: %.2f MB/s",
									p.ID, metrics.selectionCount, avgRTT, metrics.maxRTT, metrics.minRTT, lossRate*100, pathThroughput)
							}
							metrics.mu.Unlock()
						}
					}
				}
			}
		}

		// 测试结束，关闭所有goroutine
		close(stopChan)
		wg.Wait()

		// 输出最终统计信息
		log.Info("===== 测试完成 =====")
		log.Info("总测试时长: %v", time.Since(testStart))
		log.Info("总发送包数: %d", packetNumber)
		log.Info("最终活动路径数: %d", len(paths))

		// 清理资源
		pathMutex.Lock()
		for _, p := range paths {
			conn.RemovePath(p.ID)
		}
		pathMutex.Unlock()
	}()

	// 等待测试完成或超时
	select {
	case <-done:
		log.Info("测试正常完成")
	case <-time.After(testDuration):
		t.Fatal("测试超时")
	}
}

// 定义测试场景的配置结构
type pathTestConfig struct {
	baseRTT      time.Duration
	baseBW       uint64
	priority     uint8
	initialState PathState
	lossRate     float64
}

// TestMultiPathScenarios 包含多个子测试场景
func TestMultiPathScenarios(t *testing.T) {
	log.SetLogLevel("DEBUG")

	// 定义子测试场景
	scenarios := []struct {
		name        string
		description string
		duration    time.Duration
		pathConfigs []pathTestConfig
		testFunc    func(t *testing.T, conn *Connection, scheduler *PacketScheduler, paths []*Path)
	}{
		{
			name:        "高性能路径故障切换",
			description: "测试当主路径性能下降时的故障切换能力",
			duration:    30 * time.Second,
			pathConfigs: []pathTestConfig{
				{20 * time.Millisecond, 100 * 1440, 0, PathStateActive, 0.01},  // 主路径
				{50 * time.Millisecond, 50 * 1440, 1, PathStateActive, 0.05},   // 备用路径1
				{100 * time.Millisecond, 30 * 1440, 2, PathStateStandby, 0.08}, // 备用路径2
			},
			testFunc: testPathFailover,
		},
		{
			name:        "网络抖动适应性",
			description: "测试在网络条件频繁变化时的适应能力",
			duration:    30 * time.Second,
			pathConfigs: []pathTestConfig{
				{30 * time.Millisecond, 80 * 1440, 0, PathStateActive, 0.02},
				{40 * time.Millisecond, 60 * 1440, 1, PathStateActive, 0.03},
				{60 * time.Millisecond, 40 * 1440, 1, PathStateActive, 0.04},
			},
			testFunc: testNetworkJitter,
		},
		{
			name:        "拥塞控制响应",
			description: "测试在不同拥塞情况下的响应能力",
			duration:    30 * time.Second,
			pathConfigs: []pathTestConfig{
				{25 * time.Millisecond, 120 * 1440, 0, PathStateActive, 0.01},
				{45 * time.Millisecond, 90 * 1440, 1, PathStateActive, 0.03},
			},
			testFunc: testCongestionResponse,
		},
		{
			name:        "动态路径管理",
			description: "测试动态添加和移除路径的能力",
			duration:    30 * time.Second,
			pathConfigs: []pathTestConfig{
				{20 * time.Millisecond, 100 * 1440, 0, PathStateActive, 0.01},
				{40 * time.Millisecond, 70 * 1440, 1, PathStateActive, 0.03},
			},
			testFunc: testDynamicPathManagement,
		},
	}

	// 运行所有测试场景
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			log.Info("开始测试场景: %s - %s", scenario.name, scenario.description)

			config := &Config{
				MaxPaths:              16,
				PathValidationTimeout: 50 * time.Millisecond,
				EnablePathMigration:   true,
			}
			conn := NewConnection(config)
			scheduler := NewPacketScheduler(conn)

			// 初始化路径
			paths := initializeTestPaths(t, conn, scenario.pathConfigs)

			// 运行测试场景
			done := make(chan bool)
			go func() {
				scenario.testFunc(t, conn, scheduler, paths)
				done <- true
			}()

			// 等待测试完成或超时
			select {
			case <-done:
				log.Info("场景 '%s' 测试完成", scenario.name)
			case <-time.After(scenario.duration + 5*time.Second):
				t.Fatalf("场景 '%s' 测试超时", scenario.name)
			}
		})
	}
}

// 初始化测试路径
func initializeTestPaths(t *testing.T, conn *Connection, configs []pathTestConfig) []*Path {
	paths := make([]*Path, 0, len(configs))
	basePort := 1234

	for i, cfg := range configs {
		local := &net.UDPAddr{
			IP:   net.ParseIP(fmt.Sprintf("192.168.%d.1", i+1)),
			Port: basePort + i,
		}
		remote := &net.UDPAddr{
			IP:   net.ParseIP(fmt.Sprintf("192.168.%d.2", i+1)),
			Port: basePort + 1000 + i,
		}

		path, err := conn.AddPath(local, remote)
		if err != nil {
			t.Fatalf("添加路径失败: %v", err)
		}

		path.mu.Lock()
		path.Priority = cfg.priority
		path.metrics.SmoothedRTT = cfg.baseRTT
		path.metrics.LossRate = cfg.lossRate
		path.congestionWindow = cfg.baseBW
		path.State = cfg.initialState
		path.validationStatus = true
		path.mu.Unlock()

		if cfg.initialState == PathStateActive {
			conn.activePaths = append(conn.activePaths, path.ID)
		}

		paths = append(paths, path)
		log.Debug("创建测试路径 - ID: %d, RTT: %v, 带宽: %d, 优先级: %d, 状态: %v",
			path.ID, cfg.baseRTT, cfg.baseBW, cfg.priority, cfg.initialState)
	}

	return paths
}

// 测试场景1：路径故障切换
func testPathFailover(t *testing.T, conn *Connection, scheduler *PacketScheduler, paths []*Path) {
	mainPath := paths[0]

	// 记录初始吞吐量
	initialThroughput := measurePathThroughput(t, scheduler, mainPath, 5*time.Second)
	log.Info("主路径初始吞吐量: %.2f MB/s", initialThroughput)

	// 触发主路径性能下降
	time.Sleep(10 * time.Second)
	mainPath.mu.Lock()
	mainPath.metrics.LossRate = 0.3   // 增加丢包率
	mainPath.metrics.SmoothedRTT *= 3 // 增加延迟
	mainPath.mu.Unlock()
	log.Debug("触发主路径性能下降 - ID: %d", mainPath.ID)

	// 测量切换后的吞吐量
	time.Sleep(5 * time.Second)
	newThroughput := measurePathThroughput(t, scheduler, nil, 5*time.Second)
	log.Info("故障切换后系统吞吐量: %.2f MB/s", newThroughput)

	// 验证系统是否成功切换到备用路径
	if newThroughput < initialThroughput*0.5 {
		t.Errorf("故障切换后吞吐量显著下降: %.2f -> %.2f MB/s",
			initialThroughput, newThroughput)
	}
}

// 测试场景2：网络抖动适应性
func testNetworkJitter(t *testing.T, conn *Connection, scheduler *PacketScheduler, paths []*Path) {
	startTime := time.Now()
	jitterTicker := time.NewTicker(100 * time.Millisecond)
	defer jitterTicker.Stop()

	for time.Since(startTime) < 30*time.Second {
		select {
		case <-jitterTicker.C:
			for _, path := range paths {
				path.mu.Lock()
				// 随机RTT波动：±30ms
				rttDelta := time.Duration(rand.Int63n(60)-30) * time.Millisecond
				path.metrics.SmoothedRTT += rttDelta
				if path.metrics.SmoothedRTT < time.Millisecond {
					path.metrics.SmoothedRTT = time.Millisecond
				}

				// 随机丢包率波动：±2%
				lossDelta := (rand.Float64() - 0.5) * 0.04
				path.metrics.LossRate += lossDelta
				if path.metrics.LossRate < 0 {
					path.metrics.LossRate = 0
				} else if path.metrics.LossRate > 1 {
					path.metrics.LossRate = 1
				}
				path.mu.Unlock()
			}
		}
	}
}

// 测试场景3：拥塞控制响应
func testCongestionResponse(t *testing.T, conn *Connection, scheduler *PacketScheduler, paths []*Path) {
	startTime := time.Now()
	var packetCount uint64

	// 模拟突发流量
	burstTicker := time.NewTicker(5 * time.Second)
	defer burstTicker.Stop()

	for time.Since(startTime) < 30*time.Second {
		select {
		case <-burstTicker.C:
			// 触发拥塞事件
			for _, path := range paths {
				path.mu.Lock()
				originalWindow := path.congestionWindow
				path.congestionWindow = safeDiv(path.congestionWindow, 2) // 模拟拥塞窗口收缩
				log.Debug("触发拥塞事件 - 路径: %d, 窗口: %d -> %d",
					path.ID, originalWindow, path.congestionWindow)
				path.mu.Unlock()
			}
		default:
			// 持续发送数据包
			if _, _, err := scheduler.SchedulePacket(nil); err == nil {
				packetCount++

				// 记录拥塞窗口变化
				if packetCount%1000 == 0 {
					for _, p := range paths {
						p.mu.Lock()
						log.Debug("路径状态 - ID: %d, 拥塞窗口: %d, RTT: %v",
							p.ID, p.congestionWindow, p.metrics.SmoothedRTT)
						p.mu.Unlock()
					}
				}
			}
		}
	}
}

// 测试场景4：动态路径管理
func testDynamicPathManagement(t *testing.T, conn *Connection, scheduler *PacketScheduler, paths []*Path) {
	startTime := time.Now()
	pathChangeTicker := time.NewTicker(5 * time.Second)
	defer pathChangeTicker.Stop()

	basePort := 5000
	pathCounter := len(paths)

	for time.Since(startTime) < 30*time.Second {
		select {
		case <-pathChangeTicker.C:
			// 随机添加新路径
			if rand.Float64() < 0.5 && len(conn.paths) < 8 {
				pathCounter++
				local := &net.UDPAddr{
					IP:   net.ParseIP(fmt.Sprintf("192.168.%d.1", pathCounter)),
					Port: basePort + pathCounter,
				}
				remote := &net.UDPAddr{
					IP:   net.ParseIP(fmt.Sprintf("192.168.%d.2", pathCounter)),
					Port: basePort + 1000 + pathCounter,
				}

				newPath, err := conn.AddPath(local, remote)
				if err == nil {
					newPath.mu.Lock()
					newPath.Priority = uint8(rand.Intn(3))
					newPath.metrics.SmoothedRTT = time.Duration(30+rand.Intn(50)) * time.Millisecond
					newPath.metrics.LossRate = 0.01 + rand.Float64()*0.05
					newPath.congestionWindow = uint64(40+rand.Intn(40)) * 1440
					newPath.State = PathStateActive
					newPath.validationStatus = true
					newPath.mu.Unlock()

					conn.activePaths = append(conn.activePaths, newPath.ID)
					paths = append(paths, newPath)
					log.Debug("添加新路径 - ID: %d", newPath.ID)
				}
			}

			// 随机移除现有路径
			if rand.Float64() < 0.3 && len(conn.paths) > 2 {
				pathIndex := rand.Intn(len(paths))
				pathToRemove := paths[pathIndex]

				err := conn.RemovePath(pathToRemove.ID)
				if err == nil {
					paths = append(paths[:pathIndex], paths[pathIndex+1:]...)
					log.Debug("移除路径 - ID: %d", pathToRemove.ID)
				}
			}
		}
	}
}

// 辅助函数：测量路径吞吐量
func measurePathThroughput(t *testing.T, scheduler *PacketScheduler, targetPath *Path, duration time.Duration) float64 {
	startTime := time.Now()
	var totalBytes uint64
	var packetCount uint64

	for time.Since(startTime) < duration {
		if _, path, err := scheduler.SchedulePacket(nil); err == nil {
			if targetPath == nil || path.ID == targetPath.ID {
				packetCount++
				totalBytes = safeAdd(totalBytes, 1440) // 假设每个包1440字节
			}
		}
	}

	throughput := float64(totalBytes) / duration.Seconds() / 1024 / 1024 // MB/s
	return throughput
}

// TestSafeMathOperations tests the safe math operations
func TestSafeMathOperations(t *testing.T) {
	log.SetLogLevel("DEBUG")
	log.Info("开始 TestSafeMathOperations 测试")

	tests := []struct {
		name     string
		op       string
		a        uint64
		b        uint64
		expected uint64
	}{
		{"正常加法", "add", 100, 200, 300},
		{"溢出加法", "add", math.MaxUint64, 1, math.MaxUint64},
		{"正常减法", "sub", 200, 100, 100},
		{"下溢减法", "sub", 100, 200, 0},
		{"正常乘法", "mul", 100, 200, 20000},
		{"溢出乘法", "mul", math.MaxUint64, 2, math.MaxUint64},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var result uint64
			switch tt.op {
			case "add":
				result = safeAdd(tt.a, tt.b)
			case "sub":
				result = safeSub(tt.a, tt.b)
			case "mul":
				result = safeMul(tt.a, tt.b)
			}

			if result != tt.expected {
				t.Errorf("%s: %d %s %d = %d; 期望 %d",
					tt.name, tt.a, tt.op, tt.b, result, tt.expected)
			}
			log.Debug("%s: %d %s %d = %d", tt.name, tt.a, tt.op, tt.b, result)
		})
	}
}

// TestPathMetricsOverflow tests the overflow protection in PathMetrics
func TestPathMetricsOverflow(t *testing.T) {
	log.SetLogLevel("DEBUG")
	log.Info("开始 TestPathMetricsOverflow 测试")

	metrics := &PathMetrics{}

	// 测试正常字节计数
	metrics.AddBytesSent(1000)
	sent := atomic.LoadUint64(&metrics.BytesSent)
	if sent != 1000 {
		t.Errorf("期望发送字节数为 1000，实际为 %d", sent)
	}
	log.Debug("正常字节计数测试通过: %d", sent)

	// 测试溢出保护
	metrics.AddBytesSent(math.MaxUint64)
	sent = atomic.LoadUint64(&metrics.BytesSent)
	if sent != math.MaxUint64 {
		t.Errorf("期望发送字节数为 MaxUint64，实际为 %d", sent)
	}
	log.Debug("溢出保护测试通过: %d", sent)

	// 测试接收字节计数
	metrics.AddBytesReceived(2000)
	received := atomic.LoadUint64(&metrics.BytesReceived)
	if received != 2000 {
		t.Errorf("期望接收字节数为 2000，实际为 %d", received)
	}
	log.Debug("接收字节计数测试通过: %d", received)

	// 测试并发安全性
	var wg sync.WaitGroup
	concurrent := 100
	wg.Add(concurrent)
	for i := 0; i < concurrent; i++ {
		go func() {
			defer wg.Done()
			metrics.AddBytesSent(1000)
			metrics.AddBytesReceived(1000)
		}()
	}
	wg.Wait()

	sent = atomic.LoadUint64(&metrics.BytesSent)
	received = atomic.LoadUint64(&metrics.BytesReceived)
	if sent < uint64(concurrent)*1000 {
		t.Errorf("并发字节计数不准确: 发送 %d", sent)
	}
	if received < uint64(concurrent)*1000 {
		t.Errorf("并发字节计数不准确: 接收 %d", received)
	}
	log.Debug("并发安全性测试通过: 发送 %d, 接收 %d", sent, received)
}
