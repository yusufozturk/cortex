package store

import (
	"github.com/myntra/cortex/pkg/events"
	"github.com/myntra/cortex/pkg/executions"
	"github.com/myntra/cortex/pkg/rules"
)

//go:generate msgp

// DB boltdb storage
type DB struct {
	Buckets map[string]*events.Bucket     `json:"buckets"`
	Rules   map[string]*rules.Rule        `json:"rules"`
	History map[string]*executions.Record `json:"history"`
	Scripts map[string][]byte             `json:"script"`
}