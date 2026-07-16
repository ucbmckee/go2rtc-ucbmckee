package baichuan

type uidSendStats struct {
	packets     uint64
	retransmits uint64
	acks        uint64
	windowHigh  uint64
}

type uidReceiveStats struct {
	packets     uint64
	invalid     uint64
	foreign     uint64
	controls    uint64
	duplicates  uint64
	outOfWindow uint64
	acks        uint64
	windowHigh  uint64
	queueHigh   uint64
}

type TransportStats struct {
	Mode              string `json:"mode"`
	SentPackets       uint64 `json:"sent_packets,omitempty"`
	Retransmits       uint64 `json:"retransmits,omitempty"`
	ReceivedACKs      uint64 `json:"received_acks,omitempty"`
	SendWindowHigh    uint64 `json:"send_window_high_water,omitempty"`
	SendPending       uint64 `json:"send_pending,omitempty"`
	ReceivedPackets   uint64 `json:"received_packets,omitempty"`
	InvalidPackets    uint64 `json:"invalid_packets,omitempty"`
	ForeignPackets    uint64 `json:"foreign_packets,omitempty"`
	ControlPackets    uint64 `json:"control_packets,omitempty"`
	DuplicatePackets  uint64 `json:"duplicate_packets,omitempty"`
	OutOfWindow       uint64 `json:"out_of_window_packets,omitempty"`
	SentACKs          uint64 `json:"sent_acks,omitempty"`
	ReceiveWindowHigh uint64 `json:"receive_window_high_water,omitempty"`
	ReadQueueHigh     uint64 `json:"read_queue_high_water,omitempty"`
}

func (s *uidConn) TransportStats() TransportStats {
	s.sendMu.Lock()
	sent := s.sendStats
	pending := s.sendCount
	s.sendMu.Unlock()
	s.receiveMu.Lock()
	received := s.receiveStats
	s.receiveMu.Unlock()
	return TransportStats{
		Mode: "uid", SentPackets: sent.packets, Retransmits: sent.retransmits,
		ReceivedACKs: sent.acks, SendWindowHigh: sent.windowHigh, SendPending: uint64(pending),
		ReceivedPackets: received.packets, InvalidPackets: received.invalid,
		ForeignPackets: received.foreign, ControlPackets: received.controls,
		DuplicatePackets: received.duplicates,
		OutOfWindow:      received.outOfWindow, SentACKs: received.acks,
		ReceiveWindowHigh: received.windowHigh, ReadQueueHigh: received.queueHigh,
	}
}

func (s *uidConn) recordUIDInvalid() {
	s.receiveMu.Lock()
	s.receiveStats.invalid++
	s.receiveMu.Unlock()
}

func (s *uidConn) recordUIDForeign() {
	s.receiveMu.Lock()
	s.receiveStats.foreign++
	s.receiveMu.Unlock()
}

func (s *uidConn) recordUIDControl() {
	s.receiveMu.Lock()
	s.receiveStats.controls++
	s.receiveMu.Unlock()
}
