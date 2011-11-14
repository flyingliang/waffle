package waffle

import (
	"errors"
	"log"
	"net"
	"sync"
	"time"
)

type workerInfo struct {
	// wid string
	// addr string
	// port string
	ekgch chan byte
}

func newWorkerInfo() *workerInfo {
	return &workerInfo{
		ekgch: make(chan byte),
	}
}

type MasterConfig struct {
	MinWorkers             uint64
	RegisterWait           int64
	MinPartitionsPerWorker uint64
	HeartbeatInterval      int64
	HeartbeatTimeout       int64
	MaxSteps               uint64
	JobId                  string
	StartStep              uint64
}

type phaseStatus struct {
	failedWorkers []string // list of workers that have failed since phase started
}

type jobInfo struct {
	canRegister    bool
	lastCheckpoint uint64
	activeVerts    uint64
	numVerts       uint64
	sentMsgs       uint64
	totalSentMsgs  uint64
}

type recoveryInfo struct {
	errors []error
}

func (r *recoveryInfo) addError(wid string, err *RecoverableError) {
	if r.errors == nil {
		r.errors = make([]error, 0)
	}
	r.errors = append(r.errors, err)
}

type Master struct {
	node

	Config       MasterConfig
	recoveryInfo recoveryInfo
	jobInfo      jobInfo

	currPhase int
	regch     chan byte
	barrierCh chan *PhaseSummary
	superstep uint64
	startTime int64
	endTime   int64

	wInfo map[string]*workerInfo

	widFn        func(string, string) string
	checkpointFn func(uint64) bool

	// job stats
	mPhaseInfo sync.RWMutex
	pStatus    phaseStatus

	rpcServ   MasterRpcServer
	rpcClient MasterRpcClient
}

func (m *Master) EnterBarrier(summary *PhaseSummary) error {
	go func() {
		m.barrierCh <- summary
	}()
	return nil
}

// For now, this is the barrier that the workers "enter" for sync
func (m *Master) barrier(ch chan *PhaseSummary) {
	bmap := make(map[string]interface{})
	for ps := range ch {
		if m.Config.JobId != ps.JobId {
			log.Fatalf("JobId mismatch in enterBarrier")
		}
		if m.currPhase != ps.PhaseId {
			log.Fatalf("Phase mismatch in enterBarrier from worker %s", ps.WorkerId)
		}

		if ps.Error != nil {
			m.handlePhaseError(ps)
		} else {
			m.collectSummaryInfo(ps)
		}
		bmap[ps.WorkerId] = nil
		if len(bmap) == len(m.workerMap) {
			return
		}
	}
}

func (m *Master) handlePhaseError(ps *PhaseSummary) {
	if err, ok := ps.Error.(*RecoverableError); ok {
		// log the recoverable error
		log.Printf("worker %s, recoverable error: %v", ps.WorkerId, err)
		// add the error to recovery info and set the state to RECOVER (might not be needed, we can just check the recovery struct for errors?)
		m.recoveryInfo.addError(ps.WorkerId, err)
	} else {
		log.Println("worker %s, unrecoverable error: %v", ps.WorkerId, ps.Error)
		panic(ps.Error) // XXX Should actually be sending some kind of shutdown directive
	}
}

func NewMaster(addr, port string) *Master {
	m := &Master{
		regch:     make(chan byte, 1),
		barrierCh: make(chan *PhaseSummary),
		wInfo:     make(map[string]*workerInfo),
	}

	m.InitNode(addr, port)
	m.regch <- 1
	m.widFn = func(addr, port string) string {
		return net.JoinHostPort(addr, port)
	}
	m.checkpointFn = func(superstep uint64) bool {
		return false
	}
	return m
}

func (m *Master) SetWorkerIdFn(fn func(string, string) string) {
	m.widFn = fn
}

func (m *Master) SetCheckpointFn(fn func(uint64) bool) {
	m.checkpointFn = fn
}

// Zero out the stats from the last step
func (m *Master) resetJobInfo() {
	log.Println("resetting job info")
	m.jobInfo.activeVerts = 0
	m.jobInfo.sentMsgs = 0
	m.jobInfo.numVerts = 0
}

// Update the stats from the current step
func (m *Master) collectSummaryInfo(ps *PhaseSummary) {
	m.mPhaseInfo.Lock()
	m.jobInfo.activeVerts += ps.ActiveVerts
	m.jobInfo.numVerts += ps.NumVerts
	m.jobInfo.sentMsgs += ps.SentMsgs
	m.jobInfo.totalSentMsgs += m.jobInfo.sentMsgs
	m.mPhaseInfo.Unlock()
}

func (m *Master) SetRpcClient(c MasterRpcClient) {
	m.rpcClient = c
}

func (m *Master) SetRpcServer(s MasterRpcServer) {
	m.rpcServ = s
}

// Init RPC
func (m *Master) startRPC() error {
	m.rpcServ.Start(m)
	m.rpcClient.Init()
	return nil
}

func (m *Master) ekg(id string) {
	/*
		msg := &BasicMasterMsg{JobId: m.Config.JobId}
		cl, e := m.cl(id)
		if e != nil {
			panic(e.String())
		}
		info := m.wInfo[id]
		var r Resp
		for {
			call := cl.Go("Worker.Healthcheck", msg, &r, nil)
			// return or timeout
			select {
			case <-call.Done:
				if call.Error != nil {
					panic(call.Error)
				}
			case <-time.After(m.Config.HeartbeatTimeout):
				// handle fault
			}

			// wait for the next interval
			select {
			case <-info.ekgch:
				return
			case <-time.Tick(m.Config.HeartbeatInterval):
				// resetTimeout
			}
		}
	*/
}

func (m *Master) RegisterWorker(addr, port string) (string, string, error) {
	<-m.regch
	defer func() { m.regch <- 1 }()

	if !m.jobInfo.canRegister {
		// cant register, get out
		return "", "", nil
	}

	log.Printf("Attempting to register %s:%s", addr, port)

	workerId := m.widFn(addr, port)
	if _, ok := m.workerMap[workerId]; ok {
		log.Printf("%s already registered, overwriting")
	}
	m.workerMap[workerId] = net.JoinHostPort(addr, port)
	m.wInfo[workerId] = newWorkerInfo()

	log.Printf("Registered %s:%s as %s for job %s", addr, port, workerId, m.Config.JobId)
	go m.ekg(workerId)

	return workerId, m.Config.JobId, nil
}

func (m *Master) registerWorkers() error {
	log.Printf("Starting registration phase")

	m.workerMap = make(map[string]string)

	// Should do this in a more Go-ish way, maybe with a select statement?
	m.jobInfo.canRegister = true
	for timer := 0; (m.Config.MinWorkers > 0 && uint64(len(m.workerMap)) < m.Config.MinWorkers) ||
		(m.Config.RegisterWait > 0 && int64(timer) < m.Config.RegisterWait); timer += 1 * 1e9 {
		<-time.After(1 * 1e9)
	}
	m.jobInfo.canRegister = false

	if len(m.workerMap) == 0 || uint64(len(m.workerMap)) < m.Config.MinWorkers && m.Config.RegisterWait > 0 {
		return errors.New("Not enough workers registered")
	}

	log.Printf("Registration phase complete")
	return nil
}

func (m *Master) determinePartitions() {
	log.Printf("Designating partitions")

	m.partitionMap = make(map[uint64]string)
	// XXX a better set of server configurations would allow us to set min partitions per worker.
	// They could send this information at registration time.
	for i, p := 0, 0; i < int(m.Config.MinPartitionsPerWorker); i++ {
		for _, workerId := range m.workerMap {
			m.partitionMap[uint64(p)] = workerId
			p++
		}
	}

	log.Printf("Assigned %d partitions to %d workers", len(m.partitionMap), len(m.workerMap))
}

func (m *Master) pushTopology() {
	log.Printf("Distributing worker and partition information")

	topInfo := &TopologyInfo{JobId: m.Config.JobId, PartitionMap: m.partitionMap, WorkerMap: m.workerMap}
	var wg sync.WaitGroup
	for _, workerAddr := range m.workerMap {
		addr := workerAddr
		wg.Add(1)
		go func() {
			if err := m.rpcClient.PushTopology(addr, topInfo); err != nil {
				panic(err)
			}
			wg.Done()
		}()
	}
	wg.Wait()

	log.Printf("Done distributing worker and partition info")
}

func (m *Master) sendExecToAllWorkers(exec *PhaseExec) error {
	for _, workerAddr := range m.workerMap {
		addr := workerAddr
		go func() {
			if err := m.rpcClient.ExecutePhase(addr, exec); err != nil {
				panic(err)
			}
		}()
	}
	return nil
}

func (m *Master) newPhaseExec(phaseId int) *PhaseExec {
	return &PhaseExec{
		PhaseId:    phaseId,
		JobId:      m.Config.JobId,
		Superstep:  m.superstep,
		NumVerts:   m.jobInfo.numVerts,
		Checkpoint: m.checkpointFn(m.superstep),
	}
}

func (m *Master) executePhase(phaseId int) error {
	m.currPhase = phaseId
	m.resetJobInfo()
	if err := m.sendExecToAllWorkers(m.newPhaseExec(phaseId)); err != nil {
		return err
	}
	m.barrier(m.barrierCh)

	// check phase status for failures.  if there are any, reallocate the topology and move the partitions of the failed workers to other workers.
	if len(m.pStatus.failedWorkers) > 0 {
		log.Printf("detected %d failed workers", len(m.pStatus.failedWorkers))
		if err := m.handleFailedWorkers(m.pStatus.failedWorkers); err != nil {
			panic(err)
		}
	} else {
		// XXX Move this
		// If we're in the superstep phase, check checkpointFn and set lastCheckpoint
		if m.currPhase == phaseSUPERSTEP && m.checkpointFn(m.superstep) {
			m.jobInfo.lastCheckpoint = m.superstep
		}
	}
	return nil
}

func (m *Master) handleFailedWorkers(failedWorkers []string) error {
	for _, wid := range m.pStatus.failedWorkers {
		delete(m.workerMap, wid)
	}
	/*
		// register new workers, use recover timeout
		if m.Config.WaitForNewWorkers {
			if err := m.registerWorkers(); err != nil {
				// XXX we actually want to die gracefully, but until then just panic
				if toErr, ok := err.(*RegistrationTimeoutError); ok {
					log.Printf("registration timeout error, storing information")
					panic(toErr)
				} else {
					panic(err)
				}
				panic(err)
			}
		}
	*/
	if len(m.workerMap) == 0 {
		return errors.New("no workers left")
	}
	// Move the failed worker partitions to other workers
	for _, wid := range m.pStatus.failedWorkers {
		if err := m.movePartitions(wid); err != nil {
			return err
		}
	}
	return nil
}

// Move partitions of wid to another worker
func (m *Master) movePartitions(moveId string) error {
	// XXX for now, we just move the partitions for dead nodes to the first worker we get on map iteration.  Make this intelligent later.
	// Have this function return error so that we can fail if there is some kind of assignment overflow in the future heuristic
	var newOwner string
	for id := range m.workerMap {
		if id != moveId {
			newOwner = id
			break
		}
	}
	for pid, wid := range m.partitionMap {
		if wid == moveId {
			log.Printf("moving partition %d from %s to %s", pid, moveId, newOwner)
			m.partitionMap[pid] = newOwner
		}
	}
	return nil
}

// run supersteps until there are no more active vertices or queued messages
func (m *Master) compute() error {
	log.Printf("Starting computation")

	log.Printf("Active verts = %d", m.jobInfo.activeVerts)
	for m.superstep = 0; m.jobInfo.activeVerts > 0 || m.jobInfo.sentMsgs > 0; m.superstep++ {
		// XXX prepareWorkers tells the worker to cycle message queues.  We should try to get rid of it.
		log.Printf("preparing for superstep %d", m.superstep)
		m.executePhase(phaseSTEPPREPARE)
		log.Printf("starting superstep %d", m.superstep)
		m.executePhase(phaseSUPERSTEP)
	}

	log.Printf("Computation complete")
	return nil
}

// shutdown workers
func (m *Master) shutdownWorkers() error {
	/*
		if e := m.sendToAllWorkers("Worker.EndJob", &BasicMasterMsg{JobId: m.Config.JobId}, nil); e != nil {
			panic(e)
		}
		// don't wait for a notify on this call	
		log.Printf("Killing ekgs and closing worker rpc clients")
		for wid, info := range m.wInfo {
			info.ekgch <- 1
			if cl, e := m.cl(wid); e == nil {
				cl.Close()
			}
		}
	*/
	return nil
}

func (m *Master) Run() {
	m.startRPC()

	m.registerWorkers()
	m.determinePartitions()
	m.pushTopology()

	if m.Config.StartStep == 0 {
		m.executePhase(phaseLOAD1)
		m.executePhase(phaseLOAD2)
	} else {
		// This is a restart
		// Find the last checkpointed step for this job
		// Check that persisted data exists for that superstep, otherwise go to the next oldest checkpointed step
		// Tell workers to load data from that checkpoint
		// Redistribute vertices

		// rollback to the last checkpointed superstep
		m.superstep = m.Config.StartStep
		// load vertices from persistence
		m.executePhase(phaseLOAD3)
		// redistribute verts? (I think this is actually useless...)
		m.executePhase(phaseLOAD2)
		// set the superstep on workers
		m.executePhase(phaseRECOVER)
		// we should be ready to go now
	}

	m.startTime = time.Seconds()
	m.compute()
	m.endTime = time.Seconds()
	m.executePhase(phaseWRITE)
	log.Printf("compute time was %d", m.endTime-m.startTime)
	m.shutdownWorkers()
}
