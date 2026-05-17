package encoder

import (
	"encoding/binary"
	"io"
	"math"
	"sync"
	"time"
)

type MixerChannel struct {
	id         int
	buffer     []byte
	mu         sync.Mutex
	active     bool
	currentVol float64
	targetVol  float64
	fadeStep   float64
	Muted      bool
	Label      string
	restoreToken int64
}

func (c *MixerChannel) Write(p []byte) (n int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buffer = append(c.buffer, p...)
	c.active = true
	return len(p), nil
}

func (c *MixerChannel) GetVolume() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.targetVol
}

func (c *MixerChannel) SetVolume(v float64, durationSec float64) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v < 0 { v = 0 }
	if v > 2.0 { v = 2.0 } // Allow up to 200%
	
	c.restoreToken = time.Now().UnixNano()
	
	if durationSec <= 0 {
		c.targetVol = v
		c.currentVol = v
		c.fadeStep = 0
	} else {
		// Linear approach: reach target in durationSec
		// Assuming 50 ticks per second (20ms per tick)
		ticks := durationSec * 50.0
		if ticks < 1 {
			ticks = 1
		}
		c.fadeStep = math.Abs(v - c.currentVol) / ticks
		c.targetVol = v
	}
	return c.restoreToken
}

func (c *MixerChannel) RestoreVolume(v float64, durationSec float64, token int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	
	// If token doesn't match, another process has taken control of this channel's volume
	if c.restoreToken != token {
		return false
	}
	
	if v < 0 { v = 0 }
	if v > 2.0 { v = 2.0 }
	
	if durationSec <= 0 {
		c.targetVol = v
		c.currentVol = v
		c.fadeStep = 0
	} else {
		ticks := durationSec * 50.0
		if ticks < 1 {
			ticks = 1
		}
		c.fadeStep = math.Abs(v - c.currentVol) / ticks
		c.targetVol = v
	}
	return true
}

func (c *MixerChannel) SetMute(m bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Muted = m
}

func (c *MixerChannel) SetLabel(l string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Label = l
}

type ChannelStatus struct {
	ID     int     `json:"id"`
	Active bool    `json:"active"`
	Volume float64 `json:"volume"`
	Muted  bool    `json:"muted"`
	Label  string  `json:"label"`
}

type AudioMixer struct {
	Channels   []*MixerChannel
	Output     io.Writer
	mu         sync.Mutex
	running    bool
	stopChan   chan struct{}
}

func NewAudioMixer(output io.Writer, numChannels int) *AudioMixer {
	m := &AudioMixer{
		Output:     output,
		stopChan:   make(chan struct{}),
	}
	for i := 0; i < numChannels; i++ {
		m.Channels = append(m.Channels, &MixerChannel{
			id:         i,
			currentVol: 1.0,
			targetVol:  1.0,
			fadeStep:   1.0,
		})
	}
	return m
}

func (m *AudioMixer) Start() {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return
	}
	m.running = true
	m.mu.Unlock()

	go m.run()
}

func (m *AudioMixer) Stop() {
	m.mu.Lock()
	if !m.running {
		m.mu.Unlock()
		return
	}
	m.running = false
	close(m.stopChan)
	m.mu.Unlock()
}

func (m *AudioMixer) run() {
	// 44100Hz, Stereo, S16LE
	// 20ms chunk = 44100 * 2 * 2 * 0.02 = 3528 bytes
	const size = 3528
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopChan:
			return
		case <-ticker.C:
			m.mixAndWrite(size)
		}
	}
}

func (m *AudioMixer) mixAndWrite(size int) {
	mixed := make([]byte, size)
	samples := make([]int32, size/2)

	m.mu.Lock()
	duck := 1.0
	m.mu.Unlock()

	for i, ch := range m.Channels {
		ch.mu.Lock()
		if !ch.active || ch.Muted {
			ch.mu.Unlock()
			continue
		}

		// If we have data but less than 'size', pad with zeros to play the final bit
		bufLen := len(ch.buffer)
		var dataToMix []byte
		if bufLen >= size {
			dataToMix = ch.buffer[:size]
			ch.buffer = ch.buffer[size:]
		} else if bufLen > 0 {
			// Pad the last chunk
			dataToMix = make([]byte, size)
			copy(dataToMix, ch.buffer)
			ch.buffer = nil
		} else {
			ch.active = false
			ch.mu.Unlock()
			continue
		}

		if len(ch.buffer) == 0 {
			ch.active = false
		}

		// Apply exact linear fade based on fadeStep calculated from duration
		if ch.targetVol != ch.currentVol && ch.fadeStep > 0 {
			if ch.currentVol < ch.targetVol {
				ch.currentVol += ch.fadeStep
				if ch.currentVol > ch.targetVol {
					ch.currentVol = ch.targetVol
				}
			} else {
				ch.currentVol -= ch.fadeStep
				if ch.currentVol < ch.targetVol {
					ch.currentVol = ch.targetVol
				}
			}
		}

		vol := ch.currentVol
		if i > 0 {
			vol = vol * duck
		}

		for j := 0; j < size; j += 2 {
			val := int16(binary.LittleEndian.Uint16(dataToMix[j : j+2]))
			v := float64(val) * vol
			samples[j/2] += int32(v)
		}
		ch.mu.Unlock()
	}

	// Output conversion with clipping protection
	for i := 0; i < len(samples); i++ {
		val := samples[i]
		if val > 32767 { val = 32767 }
		if val < -32768 { val = -32768 }
		binary.LittleEndian.PutUint16(mixed[i*2 : i*2+2], uint16(val))
	}

	m.Output.Write(mixed)
}

func (m *AudioMixer) GetStatus() []ChannelStatus {
	status := make([]ChannelStatus, len(m.Channels))
	for i, ch := range m.Channels {
		ch.mu.Lock()
		status[i] = ChannelStatus{
			ID:     ch.id,
			Active: ch.active,
			Volume: ch.targetVol,
			Muted:  ch.Muted,
			Label:  ch.Label,
		}
		ch.mu.Unlock()
	}
	return status
}
