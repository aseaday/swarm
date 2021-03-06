package swarm

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	log "github.com/Sirupsen/logrus"
	"github.com/docker/docker/pkg/units"
	"github.com/docker/swarm/cluster"
	"github.com/docker/swarm/discovery"
	"github.com/docker/swarm/scheduler"
	"github.com/docker/swarm/scheduler/node"
	"github.com/docker/swarm/state"
	"github.com/samalba/dockerclient"
)

// Cluster is exported
type Cluster struct {
	sync.RWMutex

	eventHandler cluster.EventHandler
	engines      map[string]*cluster.Engine
	scheduler    *scheduler.Scheduler
	options      *cluster.Options
	store        *state.Store
}

// NewCluster is exported
func NewCluster(scheduler *scheduler.Scheduler, store *state.Store, options *cluster.Options) cluster.Cluster {
	log.WithFields(log.Fields{"name": "swarm"}).Debug("Initializing cluster")

	cluster := &Cluster{
		engines:   make(map[string]*cluster.Engine),
		scheduler: scheduler,
		options:   options,
		store:     store,
	}

	// get the list of entries from the discovery service
	go func() {
		d, err := discovery.New(options.Discovery, options.Heartbeat)
		if err != nil {
			log.Fatal(err)
		}

		entries, err := d.Fetch()
		if err != nil {
			log.Fatal(err)

		}
		cluster.newEntries(entries)

		go d.Watch(cluster.newEntries)
	}()

	return cluster
}

// Handle callbacks for the events
func (c *Cluster) Handle(e *cluster.Event) error {
	if c.eventHandler == nil {
		return nil
	}
	if err := c.eventHandler.Handle(e); err != nil {
		log.Error(err)
	}
	return nil
}

// RegisterEventHandler registers an event handler.
func (c *Cluster) RegisterEventHandler(h cluster.EventHandler) error {
	if c.eventHandler != nil {
		return errors.New("event handler already set")
	}
	c.eventHandler = h
	return nil
}

// CreateContainer aka schedule a brand new container into the cluster.
func (c *Cluster) CreateContainer(config *cluster.ContainerConfig, name string) (*cluster.Container, error) {
	c.scheduler.Lock()
	defer c.scheduler.Unlock()

	// check new name whether avaliable
	if cID := c.getIDFromName(name); cID != "" {
		return nil, fmt.Errorf("Conflict, The name %s is already assigned to %s. You have to delete (or rename) that container to be able to assign %s to a container again.", name, cID, name)
	}

	n, err := c.scheduler.SelectNodeForContainer(c.listNodes(), config)
	if err != nil {
		return nil, err
	}

	if nn, ok := c.engines[n.ID]; ok {
		container, err := nn.Create(config, name, true)
		if err != nil {
			return nil, err
		}

		st := &state.RequestedState{
			ID:     container.Id,
			Name:   name,
			Config: config,
		}
		return container, c.store.Add(container.Id, st)
	}

	return nil, nil
}

// RemoveContainer aka Remove a container from the cluster. Containers should
// always be destroyed through the scheduler to guarantee atomicity.
func (c *Cluster) RemoveContainer(container *cluster.Container, force bool) error {
	c.scheduler.Lock()
	defer c.scheduler.Unlock()

	if err := container.Engine.Destroy(container, force); err != nil {
		return err
	}

	if err := c.store.Remove(container.Id); err != nil {
		if err == state.ErrNotFound {
			log.Debugf("Container %s not found in the store", container.Id)
			return nil
		}
		return err
	}
	return nil
}

// Entries are Docker Engines
func (c *Cluster) newEntries(entries []*discovery.Entry) {
	for _, entry := range entries {
		go func(m *discovery.Entry) {
			if !c.hasEngine(m.String()) {
				engine := cluster.NewEngine(m.String(), c.options.OvercommitRatio)
				if err := engine.Connect(c.options.TLSConfig); err != nil {
					log.Error(err)
					return
				}
				c.Lock()

				if old, exists := c.engines[engine.ID]; exists {
					c.Unlock()
					if old.Addr != engine.Addr {
						log.Errorf("ID duplicated. %s shared by %s and %s", engine.ID, old.Addr, engine.Addr)
					} else {
						log.Debugf("node %q (name: %q) with address %q is already registered", engine.ID, engine.Name, engine.Addr)
					}
					return
				}
				c.engines[engine.ID] = engine
				if err := engine.RegisterEventHandler(c); err != nil {
					log.Error(err)
					c.Unlock()
					return
				}
				c.Unlock()

			}
		}(entry)
	}
}

func (c *Cluster) hasEngine(addr string) bool {
	c.RLock()
	defer c.RUnlock()

	for _, engine := range c.engines {
		if engine.Addr == addr {
			return true
		}
	}
	return false
}

// Images returns all the images in the cluster.
func (c *Cluster) Images() []*cluster.Image {
	c.RLock()
	defer c.RUnlock()

	out := []*cluster.Image{}
	for _, n := range c.engines {
		out = append(out, n.Images()...)
	}

	return out
}

// Image returns an image with IDOrName in the cluster
func (c *Cluster) Image(IDOrName string) *cluster.Image {
	// Abort immediately if the name is empty.
	if len(IDOrName) == 0 {
		return nil
	}

	c.RLock()
	defer c.RUnlock()
	for _, n := range c.engines {
		if image := n.Image(IDOrName); image != nil {
			return image
		}
	}

	return nil
}

// RemoveImage removes an image from the cluster
func (c *Cluster) RemoveImage(image *cluster.Image) ([]*dockerclient.ImageDelete, error) {
	c.Lock()
	defer c.Unlock()
	return image.Engine.RemoveImage(image)
}

// Pull is exported
func (c *Cluster) Pull(name string, authConfig *dockerclient.AuthConfig, callback func(what, status string)) {
	var wg sync.WaitGroup

	c.RLock()
	for _, n := range c.engines {
		wg.Add(1)

		go func(nn *cluster.Engine) {
			defer wg.Done()

			if callback != nil {
				callback(nn.Name, "")
			}
			err := nn.Pull(name, authConfig)
			if callback != nil {
				if err != nil {
					callback(nn.Name, err.Error())
				} else {
					callback(nn.Name, "downloaded")
				}
			}
		}(n)
	}
	c.RUnlock()

	wg.Wait()
}

// Load image
func (c *Cluster) Load(imageReader io.Reader, callback func(what, status string)) {
	var wg sync.WaitGroup

	c.RLock()
	pipeWriters := []*io.PipeWriter{}
	pipeReaders := []*io.PipeReader{}
	for _, n := range c.engines {
		wg.Add(1)

		pipeReader, pipeWriter := io.Pipe()
		pipeReaders = append(pipeReaders, pipeReader)
		pipeWriters = append(pipeWriters, pipeWriter)

		go func(reader *io.PipeReader, nn *cluster.Engine) {
			defer wg.Done()
			defer reader.Close()

			// call engine load image
			err := nn.Load(reader)
			if callback != nil {
				if err != nil {
					callback(nn.Name, err.Error())
				}
			}
		}(pipeReader, n)
	}

	// create multi-writer
	listWriter := []io.Writer{}
	for _, pipeW := range pipeWriters {
		listWriter = append(listWriter, pipeW)
	}
	mutiWriter := io.MultiWriter(listWriter...)

	// copy image-reader to muti-writer
	_, err := io.Copy(mutiWriter, imageReader)
	if err != nil {
		log.Error(err)
	}

	// close pipe writers
	for _, pipeW := range pipeWriters {
		pipeW.Close()
	}

	c.RUnlock()

	wg.Wait()
}

// Containers returns all the containers in the cluster.
func (c *Cluster) Containers() []*cluster.Container {
	c.RLock()
	defer c.RUnlock()

	out := []*cluster.Container{}
	for _, n := range c.engines {
		out = append(out, n.Containers()...)
	}

	return out
}

func (c *Cluster) getIDFromName(name string) string {
	// Abort immediately if the name is empty.
	if len(name) == 0 {
		return ""
	}

	c.RLock()
	defer c.RUnlock()
	for _, n := range c.engines {
		for _, c := range n.Containers() {
			for _, cname := range c.Names {
				if cname == name || cname == "/"+name {
					return c.Id
				}
			}
		}
	}
	return ""
}

// Container returns the container with IDOrName in the cluster
func (c *Cluster) Container(IDOrName string) *cluster.Container {
	// Abort immediately if the name is empty.
	if len(IDOrName) == 0 {
		return nil
	}

	c.RLock()
	defer c.RUnlock()
	for _, n := range c.engines {
		if container := n.Container(IDOrName); container != nil {
			return container
		}
	}

	return nil
}

// listNodes returns all the engines in the cluster.
func (c *Cluster) listNodes() []*node.Node {
	c.RLock()
	defer c.RUnlock()

	out := make([]*node.Node, 0, len(c.engines))
	for _, n := range c.engines {
		out = append(out, node.NewNode(n))
	}

	return out
}

// listEngines returns all the engines in the cluster.
func (c *Cluster) listEngines() []*cluster.Engine {
	c.RLock()
	defer c.RUnlock()

	out := make([]*cluster.Engine, 0, len(c.engines))
	for _, n := range c.engines {
		out = append(out, n)
	}
	return out
}

// Info is exported
func (c *Cluster) Info() [][2]string {
	info := [][2]string{
		{"\bStrategy", c.scheduler.Strategy()},
		{"\bFilters", c.scheduler.Filters()},
		{"\bNodes", fmt.Sprintf("%d", len(c.engines))},
	}

	engines := c.listEngines()
	sort.Sort(cluster.EngineSorter(engines))

	for _, engine := range engines {
		info = append(info, [2]string{engine.Name, engine.Addr})
		info = append(info, [2]string{" └ Containers", fmt.Sprintf("%d", len(engine.Containers()))})
		info = append(info, [2]string{" └ Reserved CPUs", fmt.Sprintf("%d / %d", engine.UsedCpus(), engine.TotalCpus())})
		info = append(info, [2]string{" └ Reserved Memory", fmt.Sprintf("%s / %s", units.BytesSize(float64(engine.UsedMemory())), units.BytesSize(float64(engine.TotalMemory())))})
		labels := make([]string, 0, len(engine.Labels))
		for k, v := range engine.Labels {
			labels = append(labels, k+"="+v)
		}
		sort.Strings(labels)
		info = append(info, [2]string{" └ Labels", fmt.Sprintf("%s", strings.Join(labels, ", "))})
	}

	return info
}

// RANDOMENGINE returns a random engine.
func (c *Cluster) RANDOMENGINE() (*cluster.Engine, error) {
	n, err := c.scheduler.SelectNodeForContainer(c.listNodes(), &cluster.ContainerConfig{})
	if err != nil {
		return nil, err
	}
	if n != nil {
		return c.engines[n.ID], nil
	}
	return nil, nil
}

// RenameContainer rename a container
func (c *Cluster) RenameContainer(container *cluster.Container, newName string) error {
	c.RLock()
	defer c.RUnlock()

	// check new name whether avaliable
	if cID := c.getIDFromName(newName); cID != "" {
		return fmt.Errorf("Conflict, The name %s is already assigned to %s. You have to delete (or rename) that container to be able to assign %s to a container again.", newName, cID, newName)
	}

	// call engine rename
	err := container.Engine.RenameContainer(container, newName)
	if err != nil {
		return err
	}

	// update container name in store
	st, err := c.store.Get(container.Id)
	if err != nil {
		return err
	}
	st.Name = newName
	return c.store.Replace(container.Id, st)
}
