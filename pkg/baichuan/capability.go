package baichuan

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

const (
	capabilityTokens  = "system, streaming, PTZ, IO, security, replay, disk, network, alarm, record, video, image"
	maxCapabilityBody = 256 << 10
	maxCapabilities   = 512
	maxCapabilityName = 64
)

type CapabilityAccess uint8

const (
	CapabilityReadOnly CapabilityAccess = iota + 1
	CapabilityReadWrite
)

func (a CapabilityAccess) String() string {
	switch a {
	case CapabilityReadOnly:
		return "read-only"
	case CapabilityReadWrite:
		return "read-write"
	default:
		return "none"
	}
}

// Capabilities is the bounded permission set reported for one camera channel.
type Capabilities struct {
	Channel          uint8
	ObservedChannels []uint8
	ReadOnly         []string
	ReadWrite        []string
}

func (c Capabilities) Access(name string) CapabilityAccess {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, capability := range c.ReadWrite {
		if capability == name {
			return CapabilityReadWrite
		}
	}
	for _, capability := range c.ReadOnly {
		if capability == name {
			return CapabilityReadOnly
		}
	}
	return 0
}

func (c Capabilities) clone() Capabilities {
	c.ObservedChannels = append([]uint8(nil), c.ObservedChannels...)
	c.ReadOnly = append([]string(nil), c.ReadOnly...)
	c.ReadWrite = append([]string(nil), c.ReadWrite...)
	return c
}

type capabilityExtension struct {
	XMLName  xml.Name `xml:"Extension"`
	Version  string   `xml:"version,attr"`
	Username string   `xml:"userName"`
	Token    string   `xml:"token"`
}

type capabilityModule struct {
	Channel *uint8 `xml:"channelId"`
	Value   string `xml:"abilityValue"`
}

func (c *Client) Capabilities(ctx context.Context, channel uint8) (Capabilities, error) {
	// Serialize the first query so concurrent setup cannot issue or cache duplicates.
	c.capabilityMu.Lock()
	defer c.capabilityMu.Unlock()
	if value, ok := c.capabilities[channel]; ok {
		return value.clone(), nil
	}
	if err := c.Login(ctx); err != nil {
		return Capabilities{}, err
	}
	extension, err := marshalDocument(capabilityExtension{
		Version: "1.1", Username: c.cfg.username, Token: capabilityTokens,
	})
	if err != nil {
		return Capabilities{}, fmt.Errorf("baichuan: build capability request: %w", err)
	}
	response, err := c.roundTrip(ctx, request{
		command: commandAbility, channel: channel, class: classOffset, extension: extension,
	})
	if err != nil {
		return Capabilities{}, fmt.Errorf("baichuan: query capabilities: %w", err)
	}
	value, err := parseCapabilities(response.payload, channel)
	if err != nil {
		return Capabilities{}, fmt.Errorf("baichuan: parse capabilities: %w", err)
	}
	if c.capabilities == nil {
		c.capabilities = make(map[uint8]Capabilities)
	}
	c.capabilities[channel] = value
	return value.clone(), nil
}

func parseCapabilities(body []byte, channel uint8) (Capabilities, error) {
	if len(body) > maxCapabilityBody {
		return Capabilities{}, fmt.Errorf("response exceeds %d bytes", maxCapabilityBody)
	}
	decoder := xml.NewDecoder(bytes.NewReader(body))
	access := make(map[string]CapabilityAccess)
	channels := make(map[uint8]struct{})
	found, inside := false, false
	tokens := 0
	for {
		token, err := decoder.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return Capabilities{}, err
		}
		if end, ok := token.(xml.EndElement); ok {
			if end.Name.Local == "AbilityInfo" {
				inside = false
			}
			continue
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		if start.Name.Local == "AbilityInfo" {
			found = true
			inside = true
			continue
		}
		if !inside || start.Name.Local != "subModule" {
			continue
		}
		var module capabilityModule
		if err = decoder.DecodeElement(&module, &start); err != nil {
			return Capabilities{}, err
		}
		if module.Channel != nil {
			channels[*module.Channel] = struct{}{}
			if *module.Channel != channel {
				continue
			}
		}
		for raw := module.Value; ; {
			var rest string
			var more bool
			raw, rest, more = strings.Cut(raw, ",")
			tokens++
			if tokens > maxCapabilities {
				return Capabilities{}, fmt.Errorf("capability count exceeds %d", maxCapabilities)
			}
			name, value, ok := parseCapability(raw)
			if ok && value > access[name] {
				access[name] = value
			}
			if !more {
				break
			}
			raw = rest
		}
	}
	if !found {
		return Capabilities{}, fmt.Errorf("AbilityInfo missing from response")
	}
	value := Capabilities{Channel: channel}
	for id := range channels {
		value.ObservedChannels = append(value.ObservedChannels, id)
	}
	sort.Slice(value.ObservedChannels, func(i, j int) bool {
		return value.ObservedChannels[i] < value.ObservedChannels[j]
	})
	for name, permission := range access {
		if permission == CapabilityReadWrite {
			value.ReadWrite = append(value.ReadWrite, name)
		} else {
			value.ReadOnly = append(value.ReadOnly, name)
		}
	}
	sort.Strings(value.ReadOnly)
	sort.Strings(value.ReadWrite)
	return value, nil
}

func parseCapability(raw string) (string, CapabilityAccess, bool) {
	raw = strings.TrimSpace(raw)
	index := strings.LastIndexByte(raw, '_')
	if index <= 0 || index > maxCapabilityName || index == len(raw)-1 {
		return "", 0, false
	}
	name := strings.ToLower(raw[:index])
	for _, char := range name {
		if !((char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') ||
			char == '_' || char == '-' || char == '.') {
			return "", 0, false
		}
	}
	switch strings.ToLower(raw[index+1:]) {
	case "ro":
		return name, CapabilityReadOnly, true
	case "rw":
		return name, CapabilityReadWrite, true
	default:
		return "", 0, false
	}
}
