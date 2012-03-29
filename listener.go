package waffle

import (
	"donut"
	"gozk"
	"log"
)

type waffleListener struct {
	coordinator *Coordinator

	clusterName string

	graph  *Graph
	done   chan byte
	zk     *gozk.ZooKeeper
	job    Job
	config *donut.Config
}

func (l *waffleListener) OnJoin(zk *gozk.ZooKeeper) {
	log.Println("waffle onjoin")
	l.zk = zk
	l.graph = newGraph(l.job, l.coordinator)
	l.coordinator.graph = l.graph
	l.coordinator.donutConfig = l.config
	l.coordinator.start(zk)
}

func (l *waffleListener) StartWork(workId string, data map[string]interface{}) {
	l.coordinator.startWork(workId, data)
}

func (l *waffleListener) EndWork(workId string) {

}

func (l *waffleListener) OnLeave() {
	defer func() {
		l.done <- 1
	}()
}