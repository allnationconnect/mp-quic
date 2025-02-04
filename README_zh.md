# Multipath QUIC 实现 (v1.2)

[English](README.md) | [中文](README_zh.md)

本包实现了 QUIC 多路径扩展协议，遵循 [draft-ietf-quic-multipath](https://datatracker.ietf.org/doc/draft-ietf-quic-multipath/) 规范。

## 功能特性

- 多路径管理
- 路径验证
- 连接 ID 管理
- 数据包调度
- CUBIC 拥塞控制
- 基于 RTT 的路径选择
- 自动重传机制

## 安装

```bash
go get github.com/allnationconnect/mp-quic
```

## 基本用法

### 创建连接

```go
import "github.com/allnationconnect/websocket_tunnel/mp-quic"

// 使用默认配置创建连接
conn := mpquic.NewConnection(nil)

// 或使用自定义配置
config := &mpquic.Config{
    MaxPaths:              4,              // 最大路径数
    PathValidationTimeout: 5 * time.Second, // 路径验证超时
    EnablePathMigration:   true,           // 启用路径迁移
    CongestionControl:     "cubic",        // 拥塞控制算法
}
conn := mpquic.NewConnection(config)
```

### 路径管理

```go
// 添加新路径
local := &net.UDPAddr{IP: net.ParseIP("192.168.1.1"), Port: 1234}
remote := &net.UDPAddr{IP: net.ParseIP("192.168.1.2"), Port: 5678}
path, err := conn.AddPath(local, remote)
if err != nil {
    log.Error("添加路径失败: %v", err)
}

// 开始路径验证
validationConfig := mpquic.DefaultValidationConfig()
err = path.StartValidation(validationConfig)
if err != nil {
    log.Error("开始验证失败: %v", err)
}

// 处理验证响应
response := []byte{...} // 从对端接收
result := path.HandlePathResponse(response)
if result == mpquic.PathValidationSuccess {
    log.Info("路径验证成功")
}
```

### 数据包调度

```go
// 创建数据包调度器
scheduler := mpquic.NewPacketScheduler(conn)

// 调度数据包
frames := []mpquic.Frame{...}
packet, path, err := scheduler.SchedulePacket(frames)
if err != nil {
    log.Error("数据包调度失败: %v", err)
}

// 处理确认
scheduler.HandleAck(packet.Number, time.Now())

// 处理丢包
scheduler.HandleLoss(packet.Number)
```

### 连接 ID 管理

```go
// 创建 CID 管理器
cidManager := mpquic.NewCIDManager(4, 4, 8)

// 分配本地 CID
cid, err := cidManager.AllocateLocalCID(pathID)
if err != nil {
    log.Error("分配 CID 失败: %v", err)
}

// 注册远程 CID
err = cidManager.RegisterRemoteCID(remoteCID, pathID)
if err != nil {
    log.Error("注册远程 CID 失败: %v", err)
}
```

## 配置选项

### 路径配置

- `MaxPaths`: 最大并发路径数
- `PathValidationTimeout`: 路径验证超时时间
- `EnablePathMigration`: 启用/禁用路径迁移

### 验证配置

- `Timeout`: 验证超时时间
- `MaxRetries`: 最大重试次数
- `ChallengeSize`: 验证挑战数据大小

### 拥塞控制配置

- `Beta`: CUBIC beta 参数
- `C`: CUBIC C 参数
- `InitialWindow`: 初始窗口大小
- `MinWindow`: 最小窗口大小
- `MaxWindow`: 最大窗口大小
- `FastConvergence`: 启用/禁用快速收敛

## 性能考虑

1. 路径选择
   - 基于 RTT 的评分
   - 可用带宽考虑
   - 丢包率影响
   - 路径优先级

2. 拥塞控制
   - CUBIC 算法实现
   - TCP 友好性
   - 慢启动和拥塞避免阶段

3. 重传策略
   - 最多 3 次重传尝试
   - 备选路径选择
   - 丢包率跟踪

## 日志记录

实现使用结构化日志，支持不同级别：

- DEBUG: 详细操作信息
- INFO: 一般操作状态
- WARN: 警告条件
- ERROR: 错误条件

配置日志级别：

```go
log.SetLogLevel("DEBUG")
```

## 测试

运行测试套件：

```bash
# 启用调试日志
DEBUG=1 go test -v ./mp-quic

# 运行基准测试
DEBUG=1 go test -bench=. ./mp-quic
```

## 错误处理

常见错误类型：

- `ErrTooManyPaths`: 超过最大路径数限制
- `ErrPathNotFound`: 路径未找到
- `ErrNoAvailableCIDs`: 无可用连接 ID
- `ErrTooManyRemoteCIDs`: 远程 CID 数量超限
- `ErrDuplicateCID`: 重复的连接 ID
- `ErrPathValidationFailed`: 路径验证失败
- `ErrPathValidationTimeout`: 路径验证超时

## 贡献

1. Fork 仓库
2. 创建特性分支
3. 提交更改
4. 推送到分支
5. 创建 Pull Request

## 许可证

本项目采用 GNU 宽通用公共许可证第三版 (LGPL-3.0) - 详见 [LICENSE](LICENSE) 文件。

### 许可证要点：

- ✅ 允许商业使用
- ✅ 可以自由使用和分发本软件
- ✅ 可以根据需要修改软件
- ⚠️ 如果修改了软件，您必须：
  - 将修改后的源代码以 LGPL-3.0 许可证公开
  - 包含原始版权声明
  - 说明对软件所做的重要更改
  - 包含原始源代码归属
- ⚠️ 您必须包含：
  - 代码中附带许可证副本和版权声明
  - 对本原始项目的归属说明 