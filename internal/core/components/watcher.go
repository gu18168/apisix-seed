package components

import (
	"context"
	"runtime"
	"sync"

	"github.com/api7/apisix-seed/internal/core/comm"
	"github.com/api7/apisix-seed/internal/core/entity"
	"github.com/api7/apisix-seed/internal/core/storer"
	"github.com/api7/apisix-seed/internal/discoverer"
	"github.com/api7/apisix-seed/internal/log"
	"github.com/api7/apisix-seed/internal/utils"
)

type Watcher struct {
	ctx    context.Context
	cancel context.CancelFunc

	// Limit the number of simultaneously query
	sem chan struct{}
}

func (w *Watcher) Init() {
	// the number of semaphore is referenced to https://github.com/golang/go/blob/go1.17.1/src/cmd/compile/internal/noder/noder.go#L38
	w.sem = make(chan struct{}, runtime.GOMAXPROCS(0)+10)

	// List the initial information
	for _, s := range storer.GetStores() {
		objPtrs, err := s.List(entity.ServiceFilter)
		if err != nil {
			panic("storer list error")
		}

		wg := sync.WaitGroup{}
		wg.Add(len(objPtrs))
		for _, objPtr := range objPtrs {
			w.sem <- struct{}{}
			go w.handleQuery(objPtr, s.Typ, &wg)
		}
		wg.Wait()
	}
}

func (w *Watcher) Watch() {
	w.ctx, w.cancel = context.WithCancel(context.TODO())

	// Watch for entity updates from Storer
	for _, s := range storer.GetStores() {
		go w.handleWatch(s)
	}
}

func (w *Watcher) Close() {
	w.cancel()

	for _, s := range storer.GetStores() {
		s.Unwatch()
	}
}

func (w *Watcher) handleQuery(objPtr interface{}, typ string, wg *sync.WaitGroup) {
	defer func() {
		<-w.sem
		wg.Done()
	}()

	queer := objPtr.(entity.Queer)
	query, err := encodeQuery(utils.EventAdd, typ, queer)
	if err != nil {
		log.Warnf("Watcher encode query message error: %s", err)
		return
	}

	log.Infof("Watcher query: %s", query.String())
	_ = discoverer.GetDiscoverer(queer.GetType()).Query(query)
}

func (w *Watcher) handleWatch(s *storer.GenericStore) {
	ch := s.Watch()

	for {
		select {
		case <-w.ctx.Done():
			return
		case watch := <-ch:
			values, err := watch.Decode()
			if err != nil {
				log.Warnf("Watcher decode watch message error: %s", err)
				continue
			}

			wg := sync.WaitGroup{}
			wg.Add(len(values))
			for _, val := range values {
				w.sem <- struct{}{}
				go w.handleValue(val, &wg, s)
			}
			wg.Wait()
		}
	}
}

func (w *Watcher) handleValue(val []string, wg *sync.WaitGroup, s *storer.GenericStore) {
	defer func() {
		<-w.sem
		wg.Done()
	}()

	log.Infof("Watcher handle %s event: key=%s value=%s", val[0], val[1], val[2])
	switch val[0] {
	case utils.EventAdd:
		objPtr, err := s.StringToObjPtr(val[2], val[1])
		if err != nil {
			log.Warnf("value string error: %s", err)
			return
		}
		if !entity.ServiceFilter(objPtr) {
			return
		}
		entityID := discoverer.EncodeEntityID(s.Typ, val[1])
		queer := objPtr.(entity.Queer)

		oldObjPtr, ok := s.Store(val[1], objPtr)
		if !ok {
			// Obtains a new entity with service information
			log.Infof("Watcher obtains a new entity %s with service information", entityID)
			query, err := encodeQuery(utils.EventAdd, s.Typ, queer)
			if err != nil {
				log.Warnf("Watcher encode query message error: %s", err)
				return
			}
			_ = discoverer.GetDiscoverer(queer.GetType()).Query(query)
		} else if entity.ServiceUpdate(oldObjPtr, objPtr) {
			// Updates the service information of existing entity
			log.Infof("Watcher updates the service information of existing entity %s", entityID)
			update, err := encodeUpdate(oldObjPtr.(entity.Queer), queer)
			if err != nil {
				log.Warnf("Watcher encode update message error: %s", err)
				return
			}
			_ = discoverer.GetDiscoverer(queer.GetType()).Update(update)
		} else if entity.ServiceReplace(oldObjPtr, objPtr) {
			// Replaces the service information of existing entity
			log.Infof("Watcher replaces the service information of existing entity %s", entityID)
			oldQueer := oldObjPtr.(entity.Queer)
			del, err := encodeQuery(utils.EventDelete, s.Typ, oldQueer)
			if err != nil {
				log.Warnf("Watcher encode query message error: %s", err)
				return
			}
			add, err := encodeQuery(utils.EventAdd, s.Typ, queer)
			if err != nil {
				log.Warnf("Watcher encode query message error: %s", err)
				return
			}

			_ = discoverer.GetDiscoverer(oldQueer.GetType()).Query(del)
			_ = discoverer.GetDiscoverer(queer.GetType()).Query(add)
		}
	case utils.EventDelete:
		objPtr, ok := s.Delete(val[1])
		if ok {
			entityID := discoverer.EncodeEntityID(s.Typ, val[1])
			// Deletes an existing entity
			log.Infof("Watcher deletes an existing entity %s", entityID)
			queer := objPtr.(entity.Queer)
			query, err := encodeQuery(utils.EventDelete, s.Typ, queer)
			if err != nil {
				log.Warnf("Watcher encode query message error: %s", err)
				return
			}
			_ = discoverer.GetDiscoverer(queer.GetType()).Query(query)
		}
	}
}

func encodeQuery(event, typ string, queer entity.Queer) (*comm.Query, error) {
	id, service, args := queer.Extract()
	entityID := discoverer.EncodeEntityID(typ, id)

	headerVals := []string{event, entityID, service}
	query, err := comm.NewQuery(headerVals, args)
	if err != nil {
		return nil, err
	}

	return &query, nil
}

func encodeUpdate(oldQueer, newQueer entity.Queer) (*comm.Update, error) {
	_, service, args := oldQueer.Extract()
	_, _, newArgs := newQueer.Extract()

	headerVals := []string{utils.EventUpdate, service}
	update, err := comm.NewUpdate(headerVals, args, newArgs)
	if err != nil {
		return nil, err
	}

	return &update, nil
}
