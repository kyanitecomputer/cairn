// Package ipc talks to the BootMCU Root-of-Trust over the AST2700 hardware
// mailbox (IPC1), using the shared schema/v1 protobuf IpcEnvelope.
//
// Until TrustZone is enabled the CA35 payload runs in the non-secure world, so
// this uses IPC1 sub-channel 1 (non-secure CA35). FUTURE: once the BootMCU<->PSP
// link runs inside a TEE (secure world), move to sub-channel 0 and require the
// secure world.
//
// The wire format is standard protobuf (schema/v1/ipc.proto); the BootMCU
// encodes the same envelope by hand (it cannot run buffa), so both sides
// interoperate byte-for-byte. Transport framing per 32-byte mailbox slot:
// byte 0 = protobuf length, bytes 1..=len = the IpcEnvelope.
package ipc

import (
	"errors"
	"log/slog"
	"sync/atomic"
	"time"
	"unsafe"

	"google.golang.org/protobuf/proto"

	v1 "src.kyanite.computer/schema/gen/go/schema/v1"
)

const (
	ipc1Base  uintptr = 0x14C3_9000
	subchanSz uintptr = 0x200
	nsCA35    uintptr = 1 // non-secure CA35 sub-channel

	// The CA35 sees the TX/RX halves inverted relative to the BootMCU: the RoT
	// writes replies to its TX half (+0x100) — the CA35's RX; the CA35 sends on
	// the RoT's RX half (+0x000).
	caTxHalf uintptr = 0x000
	caRxHalf uintptr = 0x100

	regTrig   = 0x00
	regEnable = 0x04
	regStatus = 0x08
	regData0  = 0x10 // message id n at +0x10 + n*0x20

	slotBytes  = 32
	maxPayload = slotBytes - 1

	// msgID is the mailbox message id used for the RoT status exchange.
	msgID = 0

	pollInterval = 500 * time.Microsecond
)

func txHalf() uintptr { return ipc1Base + nsCA35*subchanSz + caTxHalf }
func rxHalf() uintptr { return ipc1Base + nsCA35*subchanSz + caRxHalf }

// Volatile MMIO via atomic load/store so the compiler never hoists a poll and
// each access emits a real bus transaction.
func load(base uintptr, off int) uint32 {
	return atomic.LoadUint32((*uint32)(unsafe.Pointer(base + uintptr(off))))
}

func store(base uintptr, off int, v uint32) {
	atomic.StoreUint32((*uint32)(unsafe.Pointer(base + uintptr(off))), v)
}

func dataOff(id int) int { return regData0 + id*0x20 }

func writeSlot(base uintptr, id int, frame *[slotBytes]byte) {
	off := dataOff(id)
	for i := 0; i < slotBytes/4; i++ {
		w := uint32(frame[i*4]) | uint32(frame[i*4+1])<<8 |
			uint32(frame[i*4+2])<<16 | uint32(frame[i*4+3])<<24
		store(base, off+i*4, w)
	}
}

func readSlot(base uintptr, id int) [slotBytes]byte {
	var frame [slotBytes]byte
	off := dataOff(id)
	for i := 0; i < slotBytes/4; i++ {
		w := load(base, off+i*4)
		frame[i*4] = byte(w)
		frame[i*4+1] = byte(w >> 8)
		frame[i*4+2] = byte(w >> 16)
		frame[i*4+3] = byte(w >> 24)
	}
	return frame
}

// Init enables the CA35 receive side so it can accept replies from the RoT.
func Init() {
	store(rxHalf(), regEnable, 0xF)
	store(rxHalf(), regStatus, 0xF) // clear any stale pending bits
}

var seq atomic.Uint32

// Request sends one IpcEnvelope to the RoT and waits for the correlated reply.
func Request(req *v1.IpcEnvelope, timeout time.Duration) (*v1.IpcEnvelope, error) {
	pb, err := proto.MarshalOptions{Deterministic: true}.Marshal(req)
	if err != nil {
		return nil, err
	}
	if len(pb) > maxPayload {
		return nil, errors.New("ipc: envelope exceeds one mailbox slot")
	}

	var frame [slotBytes]byte
	frame[0] = byte(len(pb))
	copy(frame[1:], pb)

	deadline := time.Now().Add(timeout)
	bit := uint32(1) << uint(msgID)

	// Wait for a prior message on this id to be consumed, then send.
	for load(txHalf(), regStatus)&bit != 0 {
		if time.Now().After(deadline) {
			return nil, errors.New("ipc: tx busy timeout")
		}
		time.Sleep(pollInterval)
	}
	writeSlot(txHalf(), msgID, &frame)
	store(txHalf(), regTrig, bit)

	// Wait for the reply on the RX half.
	for load(rxHalf(), regStatus)&bit == 0 {
		if time.Now().After(deadline) {
			return nil, errors.New("ipc: rx timeout")
		}
		time.Sleep(pollInterval)
	}
	reply := readSlot(rxHalf(), msgID)
	store(rxHalf(), regStatus, bit) // acknowledge

	n := int(reply[0])
	if n == 0 || n > maxPayload {
		return nil, errors.New("ipc: bad reply length")
	}
	var env v1.IpcEnvelope
	if err := proto.Unmarshal(reply[1:1+n], &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// Ping sends a liveness probe and returns the echoed nonce.
func Ping(nonce uint32, timeout time.Duration) (uint32, error) {
	req := &v1.IpcEnvelope{
		Seq:     seq.Add(1),
		Payload: &v1.IpcEnvelope_Ping{Ping: &v1.Ping{Nonce: nonce}},
	}
	env, err := Request(req, timeout)
	if err != nil {
		return 0, err
	}
	pong, ok := env.Payload.(*v1.IpcEnvelope_Pong)
	if !ok {
		return 0, errors.New("ipc: expected Pong")
	}
	return pong.Pong.Nonce, nil
}

// GetRotStatus queries the BootMCU Root-of-Trust status.
func GetRotStatus(timeout time.Duration) (*v1.RotStatus, error) {
	req := &v1.IpcEnvelope{
		Seq:     seq.Add(1),
		Payload: &v1.IpcEnvelope_GetRotStatus{GetRotStatus: &v1.GetRotStatus{}},
	}
	env, err := Request(req, timeout)
	if err != nil {
		return nil, err
	}
	rs, ok := env.Payload.(*v1.IpcEnvelope_RotStatus_)
	if !ok {
		return nil, errors.New("ipc: expected RotStatus")
	}
	return rs.RotStatus_, nil
}

// Probe runs a one-shot liveness + status query against the RoT and logs the
// result. Bounded and non-fatal — intended for boot-time verification.
func Probe() {
	Init()

	if nonce, err := Ping(0x00C0_FFEE, 2*time.Second); err != nil {
		slog.Error("rot-ipc: ping failed", "err", err)
		return
	} else if nonce != 0x00C0_FFEE {
		slog.Error("rot-ipc: ping nonce mismatch", "got", nonce)
		return
	}
	slog.Info("rot-ipc: RoT alive (pong ok)")

	rs, err := GetRotStatus(2 * time.Second)
	if err != nil {
		slog.Error("rot-ipc: status query failed", "err", err)
		return
	}
	slog.Info("rot-ipc: RoT status",
		slog.Uint64("silicon_rev", uint64(rs.SiliconRev)),
		slog.Uint64("caliptra_flow", uint64(rs.CaliptraFlowStatus)),
		slog.Uint64("caliptra_boot", uint64(rs.CaliptraBootStatus)),
		slog.Bool("secure_boot", rs.SecureBootEnabled),
		slog.Bool("caliptra_rt_ready", rs.CaliptraRtReady),
	)
}
