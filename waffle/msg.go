package waffle

// messages passed from vert to vert
type Msg interface {
	DestVertId() string
	SetDestVertId(string)
}

type Resp int

const (
	OK = iota
	NOT_OK
)

type MsgBase struct {
	DestId string
}

func (m *MsgBase) DestVertId() string {
	return m.DestId
}

func (m *MsgBase) SetDestVertId(dest string) {
	m.DestId = dest
}

// messages between workers and master (control rpc, not waffle messages)
type CoordMsg interface {

}

type BasicWorkerMsg struct {
	Wid string
}

type BasicMasterMsg struct {
	JobId string
}

type RegisterMsg struct {
	Addr string
	Port string
}

type RegisterResp struct {
	Wid   string
	JobId string
}

type PmapMsg struct {
	BasicMasterMsg
	Pmap map[uint64]string
}

type WmapMsg struct {
	BasicMasterMsg
	Wmap map[string]string
}

type ClusterInfoMsg struct {
	BasicMasterMsg
	PmapMsg
	WmapMsg
}

type WorkerInfoMsg struct {
	Wid         string
	ActiveVerts uint64
	NumVerts    uint64
	SentMsgs    uint64
	Success     bool
}

type SuperstepMsg struct {
	BasicMasterMsg
	Superstep  uint64
	Checkpoint bool
	// Stats we need to distribute regarding the state of the graph
	NumVerts uint64
}
