package encoder

import (
	"encoding/binary"
	"io"
	"sync"
	"time"
	"fmt"
)

type MixerChannel struct {
	id     int
	buffer []byte
	mu     sync.Mutex
	active bool
	volume float64
	Muted  bool
	Label  string
}

func (c *MixerChannel) Write(p []byte) (n int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buffer = append(c.buffer, p...)
	c.active = true
	return len(p), nil
}

func (c *MixerChannel) SetVolume(v float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v < 0 { v = 0 }
	if v > 2.0 { v = 2.0 } // Allow up to 200%
	c.volume = v
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
	duckFactor float64
	wasActive  bool
	isDucking  bool
}

func NewAudioMixer(output io.Writer, numChannels int) *AudioMixer {
	m := &AudioMixer{
		Output:     output,
		stopChan:   make(chan struct{}),
		duckFactor: 1.0,
	}
	for i := 0; i < numChannels; i++ {
		m.Channels = append(m.Channels, &MixerChannel{
			id:     i,
			volume: 1.0,
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
	// Auto-ducking logic
	m.Channels[0].mu.Lock()
	announcerActive := m.Channels[0].active && len(m.Channels[0].buffer) >= size && !m.Channels[0].Muted
	announcerLabel := m.Channels[0].Label
	m.Channels[0].mu.Unlock()

	m.Channels[1].mu.Lock()
	musicLabel := m.Channels[1].Label
	m.Channels[1].mu.Unlock()

	target := 1.0
	if announcerActive {
		target = 0.2
	}

	if announcerActive && !m.wasActive {
		fmt.Printf("[Mixer] Channel 0 START play: %s\n", announcerLabel)
		m.wasActive = true
	}

	if m.duckFactor > target {
		if !m.isDucking {
			fmt.Printf("[Mixer] Channel 1 VOLUME DOWN for: %s\n", musicLabel)
			m.isDucking = true
		}
		// Ducking speed: 0.8 / 0.02 = 40 ticks * 20ms = 800ms fade-out
		m.duckFactor -= 0.02
		if m.duckFactor < target {
			m.duckFactor = target
		}
	} else if m.duckFactor < target {
		// Recovery speed: 0.8 / 0.005 = 160 ticks * 20ms = 3.2 seconds fade-in
		m.duckFactor += 0.005
		if m.duckFactor >= 1.0 {
			m.duckFactor = 1.0
			if m.isDucking {
				fmt.Printf("[Mixer] Channel 1 VOLUME UP back to normal\n")
				m.isDucking = false
			}
		}
	}

	if !announcerActive && m.wasActive {
		fmt.Printf("[Mixer] Channel 0 END play: %s\n", announcerLabel)
		m.wasActive = false
	}

	duck := m.duckFactor
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

		vol := ch.volume
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
			Volume: ch.volume,
			Muted:  ch.Muted,
			Label:  ch.Label,
		}
		ch.mu.Unlock()
	}
	return status
}
