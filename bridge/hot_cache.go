package main

import (
	"strings"
	"sync"
)

type volumePref struct {
	auto  bool
	level int
}

type companionCacheEntry struct {
	found bool
	state companionState
}

type parentCacheEntry struct {
	found    bool
	settings parentSettings
}

type hotCache struct {
	mu         sync.RWMutex
	volumes    map[string]volumePref
	voices     map[string]string
	companions map[string]companionCacheEntry
	parents    map[string]parentCacheEntry
}

func newHotCache() *hotCache {
	return &hotCache{
		volumes:    make(map[string]volumePref),
		voices:     make(map[string]string),
		companions: make(map[string]companionCacheEntry),
		parents:    make(map[string]parentCacheEntry),
	}
}

func (c *hotCache) getVolume(id string) (volumePref, bool) {
	if c == nil {
		return volumePref{}, false
	}
	c.mu.RLock()
	v, ok := c.volumes[strings.ToLower(id)]
	c.mu.RUnlock()
	return v, ok
}

func (c *hotCache) setVolume(id string, auto bool, level int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.volumes[strings.ToLower(id)] = volumePref{auto: auto, level: level}
	c.mu.Unlock()
}

func (c *hotCache) getVoice(id string) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.RLock()
	v, ok := c.voices[strings.ToLower(id)]
	c.mu.RUnlock()
	return v, ok
}

func (c *hotCache) setVoice(id, voice string) {
	if c == nil {
		return
	}
	key := strings.ToLower(id)
	c.mu.Lock()
	c.voices[key] = voice
	if entry, ok := c.companions[key]; ok {
		entry.state.Voice = voice
		c.companions[key] = entry
	}
	c.mu.Unlock()
}

func (c *hotCache) invalidateVoice(id string) {
	if c == nil {
		return
	}
	key := strings.ToLower(id)
	c.mu.Lock()
	delete(c.voices, key)
	delete(c.companions, key)
	c.mu.Unlock()
}

func (c *hotCache) getCompanion(id string) (companionCacheEntry, bool) {
	if c == nil {
		return companionCacheEntry{}, false
	}
	c.mu.RLock()
	v, ok := c.companions[strings.ToLower(id)]
	c.mu.RUnlock()
	return v, ok
}

func (c *hotCache) setCompanion(id string, entry companionCacheEntry) {
	if c == nil {
		return
	}
	key := strings.ToLower(id)
	c.mu.Lock()
	c.companions[key] = entry
	if entry.state.Voice != "" {
		c.voices[key] = entry.state.Voice
	}
	c.mu.Unlock()
}

func (c *hotCache) invalidateCompanion(id string) {
	if c == nil {
		return
	}
	key := strings.ToLower(id)
	c.mu.Lock()
	delete(c.companions, key)
	delete(c.voices, key)
	c.mu.Unlock()
}

func (c *hotCache) getParentSettings(id string) (parentCacheEntry, bool) {
	if c == nil {
		return parentCacheEntry{}, false
	}
	c.mu.RLock()
	v, ok := c.parents[strings.ToLower(id)]
	c.mu.RUnlock()
	return v, ok
}

func (c *hotCache) setParentSettings(id string, found bool, s parentSettings) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.parents[strings.ToLower(id)] = parentCacheEntry{found: found, settings: s}
	c.mu.Unlock()
}
