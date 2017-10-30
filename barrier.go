package kapacitor

import (
	"errors"
	"time"

	"sync"
	"sync/atomic"

	"github.com/influxdata/kapacitor/edge"
	"github.com/influxdata/kapacitor/models"
	"github.com/influxdata/kapacitor/pipeline"
)

type BarrierNode struct {
	node
	b              *pipeline.BarrierNode
	barrierStopper map[models.GroupID]func()
}

// Create a new  BarrierNode, which emits a barrier if data traffic has been idle for the configured amount of time.
func newBarrierNode(et *ExecutingTask, n *pipeline.BarrierNode, d NodeDiagnostic) (*BarrierNode, error) {
	if n.Idle == 0 && n.Period == 0 {
		return nil, errors.New("barrier node must have either a non zero idle or a non zero period")
	}
	bn := &BarrierNode{
		node:           node{Node: n, et: et, diag: d},
		b:              n,
		barrierStopper: map[models.GroupID]func(){},
	}
	bn.node.runF = bn.runBarrierEmitter
	return bn, nil
}

func (n *BarrierNode) runBarrierEmitter([]byte) error {
	defer n.stopBarrierEmitter()
	consumer := edge.NewGroupedConsumer(n.ins[0], n)
	n.statMap.Set(statCardinalityGauge, consumer.CardinalityVar())
	return consumer.Consume()
}

func (n *BarrierNode) stopBarrierEmitter() {
	for _, stopF := range n.barrierStopper {
		stopF()
	}
}

func (n *BarrierNode) NewGroup(group edge.GroupInfo, first edge.PointMeta) (edge.Receiver, error) {
	r, stopF, err := n.newBarrier(group, first)
	if err != nil {
		return nil, err
	}
	n.barrierStopper[group.ID] = stopF
	return edge.NewReceiverFromForwardReceiverWithStats(
		n.outs,
		edge.NewTimedForwardReceiver(n.timer, r),
	), nil
}

func (n *BarrierNode) newBarrier(group edge.GroupInfo, first edge.PointMeta) (edge.ForwardReceiver, func(), error) {
	switch {
	case n.b.Idle != 0:
		idleBarrier := newIdleBarrier(
			first.Name(),
			group,
			n.b.Idle,
			n.outs,
		)
		return idleBarrier, idleBarrier.Stop, nil
	case n.b.Period != 0:
		periodicBarrier := newPeriodicBarrier(
			first.Name(),
			group,
			n.b.Period,
			n.outs,
		)
		return periodicBarrier, periodicBarrier.Stop, nil
	default:
		return nil, nil, errors.New("unreachable code, barrier node should have non-zero idle or non-zero period")
	}
}

type idleBarrier struct {
	name  string
	group edge.GroupInfo

	idle  time.Duration
	lastT atomic.Value
	timer *time.Timer
	wg    sync.WaitGroup
	outs  []edge.StatsEdge
	stopC chan interface{}
}

func newIdleBarrier(name string, group edge.GroupInfo, idle time.Duration, outs []edge.StatsEdge) *idleBarrier {
	r := &idleBarrier{
		name:  name,
		group: group,
		idle:  idle,
		lastT: atomic.Value{},
		timer: time.NewTimer(idle),
		wg:    sync.WaitGroup{},
		outs:  outs,
		stopC: make(chan interface{}, 1),
	}

	r.Init()

	return r
}

func (n *idleBarrier) Init() {
	n.lastT.Store(time.Time{})
	n.wg.Add(1)

	go n.idleHandler()
}

func (n *idleBarrier) Stop() {
	close(n.stopC)
	n.timer.Stop()
	n.wg.Wait()
}

func (n *idleBarrier) BeginBatch(m edge.BeginBatchMessage) (edge.Message, error) {
	return m, nil
}
func (n *idleBarrier) BatchPoint(m edge.BatchPointMessage) (edge.Message, error) {
	if !m.Time().Before(n.lastT.Load().(time.Time)) {
		n.resetTimer()
		return m, nil
	}
	return nil, nil
}
func (n *idleBarrier) EndBatch(m edge.EndBatchMessage) (edge.Message, error) {
	return m, nil
}
func (n *idleBarrier) Barrier(m edge.BarrierMessage) (edge.Message, error) {
	if !m.Time().Before(n.lastT.Load().(time.Time)) {
		n.resetTimer()
		return m, nil
	}
	return nil, nil
}
func (n *idleBarrier) DeleteGroup(m edge.DeleteGroupMessage) (edge.Message, error) {
	if m.GroupID() == n.group.ID {
		n.Stop()
	}
	return m, nil
}

func (n *idleBarrier) Point(m edge.PointMessage) (edge.Message, error) {
	if !m.Time().Before(n.lastT.Load().(time.Time)) {
		n.resetTimer()
		return m, nil
	}
	return nil, nil
}

func (n *idleBarrier) resetTimer() {
	n.timer.Reset(n.idle)
}

func (n *idleBarrier) emitBarrier() error {
	nowT := time.Now()
	n.lastT.Store(nowT)
	return edge.Forward(n.outs, edge.NewBarrierMessage(n.group, nowT))
}

func (n *idleBarrier) idleHandler() {
	defer n.wg.Done()
	for {
		select {
		case <-n.timer.C:
			n.emitBarrier()
			n.resetTimer()
		case <-n.stopC:
			return
		}
	}
}

type periodicBarrier struct {
	name  string
	group edge.GroupInfo

	lastT  atomic.Value
	ticker *time.Ticker
	wg     sync.WaitGroup
	outs   []edge.StatsEdge
	stopC  chan bool
}

func newPeriodicBarrier(name string, group edge.GroupInfo, period time.Duration, outs []edge.StatsEdge) *periodicBarrier {
	r := &periodicBarrier{
		name:   name,
		group:  group,
		lastT:  atomic.Value{},
		ticker: time.NewTicker(period),
		wg:     sync.WaitGroup{},
		outs:   outs,
		stopC:  make(chan bool, 1),
	}

	r.Init()

	return r
}

func (n *periodicBarrier) Init() {
	n.lastT.Store(time.Time{})
	n.wg.Add(1)

	go n.periodicEmitter()
}

func (n *periodicBarrier) Stop() {
	n.stopC <- true
	n.ticker.Stop()
	n.wg.Wait()
}

func (n *periodicBarrier) BeginBatch(m edge.BeginBatchMessage) (edge.Message, error) {
	return m, nil
}
func (n *periodicBarrier) BatchPoint(m edge.BatchPointMessage) (edge.Message, error) {
	if !m.Time().Before(n.lastT.Load().(time.Time)) {
		return m, nil
	}
	return nil, nil
}
func (n *periodicBarrier) EndBatch(m edge.EndBatchMessage) (edge.Message, error) {
	return m, nil
}
func (n *periodicBarrier) Barrier(m edge.BarrierMessage) (edge.Message, error) {
	if !m.Time().Before(n.lastT.Load().(time.Time)) {
		return m, nil
	}
	return nil, nil
}
func (n *periodicBarrier) DeleteGroup(m edge.DeleteGroupMessage) (edge.Message, error) {
	if m.GroupID() == n.group.ID {
		n.Stop()
	}
	return m, nil
}

func (n *periodicBarrier) Point(m edge.PointMessage) (edge.Message, error) {
	if !m.Time().Before(n.lastT.Load().(time.Time)) {
		return m, nil
	}
	return nil, nil
}

func (n *periodicBarrier) emitBarrier() error {
	nowT := time.Now()
	n.lastT.Store(nowT)
	return edge.Forward(n.outs, edge.NewBarrierMessage(n.group, nowT))
}

func (n *periodicBarrier) periodicEmitter() {
	defer n.wg.Done()
	for {
		select {
		case <-n.ticker.C:
			n.emitBarrier()
		case <-n.stopC:
			return
		}
	}
}
