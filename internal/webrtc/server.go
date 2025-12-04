package webrtc

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/pion/webrtc/v3"

	"home-monitor/internal/capture"
	"home-monitor/internal/config"
)

// PeerConnection WebRTC 连接
type PeerConnection struct {
	ID       string
	CameraID string
	PC       *webrtc.PeerConnection
	Done     chan struct{}
}

// Server WebRTC 服务器（使用 RTP 转发）
type Server struct {
	captureManager *capture.Manager
	cameras        map[string]config.CameraConfig
	forwarders     map[string]*RTPForwarder
	connections    map[string]*PeerConnection
	frameFeeds     map[string]context.CancelFunc // 帧订阅的取消函数

	mutex  sync.RWMutex
	config webrtc.Configuration

	// 端口分配
	nextVideoPort int
	nextAudioPort int
	portMutex     sync.Mutex

	// 服务器 context
	ctx    context.Context
	cancel context.CancelFunc
}

// NewServer 创建 WebRTC 服务器
func NewServer(captureManager *capture.Manager, cameras []config.CameraConfig, stunServers []string) *Server {
	if len(stunServers) == 0 {
		stunServers = []string{
			"stun:stun.l.google.com:19302",
			"stun:stun1.l.google.com:19302",
		}
	}

	cfg := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: stunServers},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())

	s := &Server{
		captureManager: captureManager,
		cameras:        make(map[string]config.CameraConfig),
		forwarders:     make(map[string]*RTPForwarder),
		connections:    make(map[string]*PeerConnection),
		frameFeeds:     make(map[string]context.CancelFunc),
		config:         cfg,
		nextVideoPort:  5000,
		nextAudioPort:  5100,
		ctx:            ctx,
		cancel:         cancel,
	}

	// 保存摄像头配置
	for _, cam := range cameras {
		if cam.Enabled {
			s.cameras[cam.ID] = cam
		}
	}

	return s
}

// allocatePorts 分配端口
func (s *Server) allocatePorts() (int, int) {
	s.portMutex.Lock()
	defer s.portMutex.Unlock()

	videoPort := s.nextVideoPort
	audioPort := s.nextAudioPort
	s.nextVideoPort += 2
	s.nextAudioPort += 2

	return videoPort, audioPort
}

// getOrCreateForwarder 获取或创建 RTP 转发器
func (s *Server) getOrCreateForwarder(ctx context.Context, cameraID string) (*RTPForwarder, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	// 检查是否已存在
	if fwd, exists := s.forwarders[cameraID]; exists {
		if fwd.IsRunning() {
			return fwd, nil
		}
		// 已停止，删除旧的
		if cancelFn, ok := s.frameFeeds[cameraID]; ok {
			cancelFn()
			delete(s.frameFeeds, cameraID)
		}
		delete(s.forwarders, cameraID)
	}

	// 获取摄像头配置
	camConfig, exists := s.cameras[cameraID]
	if !exists {
		return nil, fmt.Errorf("摄像头不存在: %s", cameraID)
	}

	// 获取采集器
	capturer, err := s.captureManager.GetCapturer(cameraID)
	if err != nil {
		return nil, fmt.Errorf("获取采集器失败: %w", err)
	}

	if !capturer.IsRunning() {
		return nil, fmt.Errorf("采集器未运行: %s", cameraID)
	}

	// 分配端口
	videoPort, audioPort := s.allocatePorts()

	// 创建转发器
	fwd := NewRTPForwarder(cameraID, camConfig, videoPort, audioPort)

	// 启动转发器
	if err := fwd.Start(ctx); err != nil {
		return nil, err
	}

	// 订阅帧流并喂给转发器
	feedCtx, feedCancel := context.WithCancel(ctx)
	s.frameFeeds[cameraID] = feedCancel

	subID := fmt.Sprintf("webrtc_%s_%d", cameraID, time.Now().UnixNano())
	frameCh := capturer.SubscribeFrames(subID)

	go func() {
		defer capturer.UnsubscribeFrames(subID)
		frameCount := 0
		for {
			select {
			case <-feedCtx.Done():
				log.Printf("帧订阅已取消: %s", cameraID)
				return
			case frame, ok := <-frameCh:
				if !ok {
					log.Printf("帧通道已关闭: %s", cameraID)
					return
				}
				frameCount++
				if frameCount <= 5 || frameCount%100 == 0 {
					log.Printf("收到帧 #%d (%d bytes)，发送到转发器: %s", frameCount, len(frame), cameraID)
				}
				fwd.WriteFrame(frame)
			}
		}
	}()

	// 如果启用音频，也订阅音频流
	if camConfig.Audio.Enabled {
		audioCh := capturer.SubscribeAudio(subID + "_audio")

		go func() {
			defer capturer.UnsubscribeAudio(subID + "_audio")
			audioCount := 0
			for {
				select {
				case <-feedCtx.Done():
					log.Printf("音频订阅已取消: %s", cameraID)
					return
				case audio, ok := <-audioCh:
					if !ok {
						log.Printf("音频通道已关闭: %s", cameraID)
						return
					}
					audioCount++
					if audioCount == 1 || audioCount%500 == 0 {
						log.Printf("收到音频帧 #%d (%d bytes)，发送到转发器: %s", audioCount, len(audio), cameraID)
					}
					fwd.WriteAudio(audio)
				}
			}
		}()
	}

	s.forwarders[cameraID] = fwd
	return fwd, nil
}

// OfferRequest SDP Offer 请求
type OfferRequest struct {
	CameraID string `json:"camera_id"`
	SDP      string `json:"sdp"`
}

// AnswerResponse SDP Answer 响应
type AnswerResponse struct {
	SDP          string `json:"sdp"`
	ConnectionID string `json:"connection_id"`
}

// ICECandidateMessage ICE 候选消息
type ICECandidateMessage struct {
	ConnectionID string `json:"connection_id"`
	Candidate    string `json:"candidate"`
}

// HandleOffer 处理 WebRTC Offer
func (s *Server) HandleOffer(ctx context.Context, cameraID string, offerSDP string) (string, string, error) {
	// 获取或创建 RTP 转发器（使用服务器的长期 context，而不是请求的 context）
	fwd, err := s.getOrCreateForwarder(s.ctx, cameraID)
	if err != nil {
		return "", "", err
	}

	// 创建 PeerConnection
	pc, err := webrtc.NewPeerConnection(s.config)
	if err != nil {
		return "", "", fmt.Errorf("创建 PeerConnection 失败: %w", err)
	}

	// 生成连接ID
	connID := fmt.Sprintf("webrtc_%s_%d", cameraID, time.Now().UnixNano())

	// 创建连接对象
	peerConn := &PeerConnection{
		ID:       connID,
		CameraID: cameraID,
		PC:       pc,
		Done:     make(chan struct{}),
	}

	// 添加视频轨道
	videoTrack := fwd.GetVideoTrack()
	if videoTrack != nil {
		rtpSender, err := pc.AddTrack(videoTrack)
		if err != nil {
			pc.Close()
			return "", "", fmt.Errorf("添加视频轨道失败: %w", err)
		}

		// 处理 RTCP
		go func() {
			rtcpBuf := make([]byte, 1500)
			for {
				select {
				case <-peerConn.Done:
					return
				default:
				}
				if _, _, err := rtpSender.Read(rtcpBuf); err != nil {
					return
				}
			}
		}()
	}

	// 添加音频轨道
	audioTrack := fwd.GetAudioTrack()
	if audioTrack != nil {
		rtpSender, err := pc.AddTrack(audioTrack)
		if err != nil {
			pc.Close()
			return "", "", fmt.Errorf("添加音频轨道失败: %w", err)
		}

		// 处理 RTCP
		go func() {
			rtcpBuf := make([]byte, 1500)
			for {
				select {
				case <-peerConn.Done:
					return
				default:
				}
				if _, _, err := rtpSender.Read(rtcpBuf); err != nil {
					return
				}
			}
		}()
	}

	// 设置连接状态回调
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("WebRTC 连接状态变更 [%s]: %s", connID, state.String())
		if state == webrtc.PeerConnectionStateFailed ||
			state == webrtc.PeerConnectionStateClosed ||
			state == webrtc.PeerConnectionStateDisconnected {
			s.CloseConnection(connID)
		}
	})

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Printf("ICE 连接状态变更 [%s]: %s", connID, state.String())
	})

	// 解析 Offer SDP
	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  offerSDP,
	}

	if err := pc.SetRemoteDescription(offer); err != nil {
		pc.Close()
		return "", "", fmt.Errorf("设置远程描述失败: %w", err)
	}

	// 创建 Answer
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		pc.Close()
		return "", "", fmt.Errorf("创建 Answer 失败: %w", err)
	}

	// 设置本地描述
	if err := pc.SetLocalDescription(answer); err != nil {
		pc.Close()
		return "", "", fmt.Errorf("设置本地描述失败: %w", err)
	}

	// 等待 ICE 候选收集完成
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	<-gatherComplete

	// 保存连接
	s.mutex.Lock()
	s.connections[connID] = peerConn
	s.mutex.Unlock()

	// 增加订阅者
	fwd.AddSubscriber()

	log.Printf("WebRTC 连接已建立: %s (摄像头: %s, 订阅者: %d)", connID, cameraID, fwd.GetSubscriberCount())

	return pc.LocalDescription().SDP, connID, nil
}

// AddICECandidate 添加 ICE 候选
func (s *Server) AddICECandidate(connID string, candidateJSON string) error {
	s.mutex.RLock()
	conn, exists := s.connections[connID]
	s.mutex.RUnlock()

	if !exists {
		return fmt.Errorf("连接不存在: %s", connID)
	}

	var candidate webrtc.ICECandidateInit
	if err := json.Unmarshal([]byte(candidateJSON), &candidate); err != nil {
		return fmt.Errorf("解析 ICE 候选失败: %w", err)
	}

	if err := conn.PC.AddICECandidate(candidate); err != nil {
		return fmt.Errorf("添加 ICE 候选失败: %w", err)
	}

	return nil
}

// CloseConnection 关闭连接
func (s *Server) CloseConnection(connID string) error {
	s.mutex.Lock()
	conn, exists := s.connections[connID]
	if exists {
		delete(s.connections, connID)
	}
	s.mutex.Unlock()

	if !exists {
		return nil
	}

	// 关闭 done channel
	select {
	case <-conn.Done:
	default:
		close(conn.Done)
	}

	// 关闭 PeerConnection
	if err := conn.PC.Close(); err != nil {
		log.Printf("关闭 PeerConnection 失败: %v", err)
	}

	// 减少转发器订阅者
	s.mutex.RLock()
	fwd, exists := s.forwarders[conn.CameraID]
	s.mutex.RUnlock()

	if exists {
		remaining := fwd.RemoveSubscriber()
		log.Printf("WebRTC 连接已关闭: %s (剩余订阅者: %d)", connID, remaining)

		// 如果没有订阅者了，停止转发器以释放 FFmpeg 资源
		if remaining <= 0 {
			log.Printf("没有剩余订阅者，停止 RTP 转发器: %s", conn.CameraID)
			fwd.Stop()

			// 从 forwarders map 中删除
			s.mutex.Lock()
			delete(s.forwarders, conn.CameraID)
			// 取消帧订阅
			if cancelFn, ok := s.frameFeeds[conn.CameraID]; ok {
				cancelFn()
				delete(s.frameFeeds, conn.CameraID)
			}
			s.mutex.Unlock()
		}
	}

	return nil
}

// GetConnectionCount 获取连接数
func (s *Server) GetConnectionCount() int {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return len(s.connections)
}

// CloseAll 关闭所有连接
func (s *Server) CloseAll() {
	// 取消服务器 context
	if s.cancel != nil {
		s.cancel()
	}

	s.mutex.Lock()
	connIDs := make([]string, 0, len(s.connections))
	for id := range s.connections {
		connIDs = append(connIDs, id)
	}
	s.mutex.Unlock()

	for _, id := range connIDs {
		s.CloseConnection(id)
	}

	// 停止所有转发器
	s.mutex.Lock()
	for _, fwd := range s.forwarders {
		fwd.Stop()
	}
	s.forwarders = make(map[string]*RTPForwarder)
	s.mutex.Unlock()
}
