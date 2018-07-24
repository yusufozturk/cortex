package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/satori/go.uuid"

	"github.com/golang/glog"
	"github.com/hashicorp/raft"
	"github.com/myntra/cortex/pkg/config"
	"github.com/myntra/cortex/pkg/events"
	"github.com/myntra/cortex/pkg/executions"
	"github.com/myntra/cortex/pkg/rules"

	"github.com/myntra/cortex/pkg/js"
	"github.com/myntra/cortex/pkg/util"
)

const (
	retainSnapshotCount = 2
	raftTimeout         = 10 * time.Second
)

type command struct {
	Op       string             `json:"op"` // stash or evict
	Rule     *rules.Rule        `json:"rule,omitempty"`
	RuleID   string             `json:"ruleID,omitempty"`
	Event    *events.Event      `json:"event,omitempty"`
	ScriptID string             `json:"script_id,omitempty"`
	Script   []byte             `json:"script,omitempty"`
	Record   *executions.Record `json:"record,omitempty"`
	RecordID string             `json:"record_id,omitempty"`
}

type defaultStore struct {
	opt *config.Config

	raft                 *raft.Raft
	scriptStorage        *scriptStorage
	bucketStorage        *bucketStorage
	executionStorage     *executionStorage
	executionBucketQueue chan *events.Bucket
	quitFlusherChan      chan struct{}
}

func newStore(opt *config.Config) (*defaultStore, error) {

	store := &defaultStore{
		scriptStorage: &scriptStorage{
			m: make(map[string][]byte),
		},
		executionStorage: &executionStorage{
			m: make(map[string]*executions.Record),
		},
		bucketStorage: &bucketStorage{
			es: &eventStorage{
				m: make(map[string]*events.Bucket),
			},
			rs: &ruleStorage{
				m: make(map[string]*rules.Rule),
			},
		},
		opt:                  opt,
		quitFlusherChan:      make(chan struct{}),
		executionBucketQueue: make(chan *events.Bucket, 1000),
	}

	if err := store.open(); err != nil {
		return nil, err
	}

	go store.flusher()

	return store, nil
}

func (d *defaultStore) executor() {
	for {
		select {
		case rb := <-d.executionBucketQueue:
			glog.Infof("received bucket %+v", rb)
			go func(rb *events.Bucket) {
				statusCode := 0
				var noScriptResult bool
				result := js.Execute(d.getScript(rb.Rule.ScriptID), rb)
				if result == nil {
					noScriptResult = true
				}
				if noScriptResult {
					statusCode = util.RetryPost(rb, rb.Rule.HookEndpoint, rb.Rule.HookRetry)
				} else {
					statusCode = util.RetryPost(result, rb.Rule.HookEndpoint, rb.Rule.HookRetry)
				}

				id, _ := uuid.NewV4()
				record := &executions.Record{
					ID:             id.String(),
					Bucket:         *rb,
					ScriptResult:   result,
					HookStatusCode: statusCode,
					CreatedAt:      time.Now(),
				}

				d.addRecord(record)

			}(rb)
		}
	}
}

func (d *defaultStore) flusher() {

	go d.executor()

	ticker := time.NewTicker(time.Second)
loop:
	for {
		select {
		case <-ticker.C:
			glog.Info("rule flusher ==> ticker called")
			for ruleID, bucket := range d.bucketStorage.es.clone() {
				glog.Info("rule flusher ==> ", ruleID, bucket.CanFlush())
				if bucket.CanFlush() {
					go func() {
						glog.Infof("post bucket to hook %+v ", bucket)
						d.executionBucketQueue <- bucket

						err := d.flushBucket(ruleID)
						if err != nil {
							glog.Errorf("error flushing %v", err)
						}
					}()
				}
			}
		case <-d.quitFlusherChan:
			break loop
		}
	}

}

func (d *defaultStore) expirer() {
	ticker := time.NewTicker(time.Hour)

	for {
		select {
		case <-ticker.C:
			if d.executionStorage.getTotalRecordsCount() > d.opt.MaxHistory {
				// TODO, remove oldest records
			}
		}

	}
}

func (d *defaultStore) applyCMD(cmd *command) error {
	if d.raft.State() != raft.Leader {
		return fmt.Errorf("not leader")
	}

	b, err := json.Marshal(cmd)
	if err != nil {
		return err
	}

	f := d.raft.Apply(b, raftTimeout)
	return f.Error()
}

func (d *defaultStore) matchAndStash(event *events.Event) error {
	for _, rule := range d.getRules() {
		go d.match(rule, event)
	}
	return nil
}

func (d *defaultStore) match(rule *rules.Rule, event *events.Event) error {
	if util.PatternMatch(event.EventType, rule.EventTypes) {
		go d.stash(rule.ID, event)
	}
	return nil
}

func (d *defaultStore) stash(ruleID string, event *events.Event) error {
	return d.applyCMD(&command{
		Op:     "stash",
		RuleID: ruleID,
		Event:  event,
	})
}

func (d *defaultStore) addRule(rule *rules.Rule) error {

	if rule.WaitWindow == 0 || rule.WaitWindowThreshold == 0 || rule.MaxWaitWindow == 0 {
		rule.WaitWindow = d.opt.DefaultWaitWindow
		rule.WaitWindowThreshold = d.opt.DefaultWaitWindowThreshold
		rule.MaxWaitWindow = d.opt.DefaultMaxWaitWindow
	}

	return d.applyCMD(&command{
		Op:   "add_rule",
		Rule: rule,
	})
}

func (d *defaultStore) updateRule(rule *rules.Rule) error {
	return d.applyCMD(&command{
		Op:   "update_rule",
		Rule: rule,
	})
}

func (d *defaultStore) addScript(id string, script []byte) error {
	return d.applyCMD(&command{
		Op:       "add_script",
		ScriptID: id,
		Script:   script,
	})
}

func (d *defaultStore) updateScript(id string, script []byte) error {
	return d.applyCMD(&command{
		Op:       "update_script",
		ScriptID: id,
		Script:   script,
	})
}

func (d *defaultStore) removeScript(id string) error {
	return d.applyCMD(&command{
		Op:       "remove_script",
		ScriptID: id,
	})
}

func (d *defaultStore) removeRule(ruleID string) error {
	return d.applyCMD(&command{
		Op:     "remove_rule",
		RuleID: ruleID,
	})
}

func (d *defaultStore) flushBucket(ruleID string) error {
	return d.applyCMD(&command{
		Op:     "flush_bucket",
		RuleID: ruleID,
	})
}

func (d *defaultStore) addRecord(r *executions.Record) error {
	return d.applyCMD(&command{
		Op:     "add_record",
		Record: r,
	})
}

func (d *defaultStore) removeRecord(id string) error {
	return d.applyCMD(&command{
		Op:       "remove_record",
		RecordID: id,
	})
}

func (d *defaultStore) getScripts() []string {
	return d.scriptStorage.getScripts()
}

func (d *defaultStore) getScript(id string) []byte {
	return d.scriptStorage.getScript(id)
}

func (d *defaultStore) getRules() []*rules.Rule {
	return d.bucketStorage.rs.getRules()
}

func (d *defaultStore) getRule(ruleID string) *rules.Rule {
	return d.bucketStorage.rs.getRule(ruleID)
}

func (d *defaultStore) getRecords(ruleID string) []*executions.Record {
	return d.executionStorage.getRecords(ruleID)
}

func (d *defaultStore) getRecordsCount(ruleID string) int {
	return d.executionStorage.getRecordsCount(ruleID)
}
