package state

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	swanevent "github.com/Dataman-Cloud/swan/src/manager/event"
	"github.com/Dataman-Cloud/swan/src/manager/framework/mesos_connector"
	"github.com/Dataman-Cloud/swan/src/manager/framework/store"
	"github.com/Dataman-Cloud/swan/src/manager/swancontext"
	"github.com/Dataman-Cloud/swan/src/types"
	"github.com/Dataman-Cloud/swan/src/utils"

	"github.com/Sirupsen/logrus"
	"golang.org/x/net/context"
)

type AppMode string

var (
	APP_MODE_FIXED      AppMode = "fixed"
	APP_MODE_REPLICATES AppMode = "replicates"
)

const (
	APP_STATE_NORMAL                 = "normal"
	APP_STATE_MARK_FOR_CREATING      = "creating"
	APP_STATE_MARK_FOR_DELETION      = "deleting"
	APP_STATE_MARK_FOR_UPDATING      = "updating"
	APP_STATE_MARK_FOR_CANCEL_UPDATE = "cancel_update"
	APP_STATE_MARK_FOR_SCALE_UP      = "scale_up"
	APP_STATE_MARK_FOR_SCALE_DOWN    = "scale_down"
)

var persistentStore store.Store

func SetStore(newStore store.Store) {
	persistentStore = newStore
}

type App struct {
	// app name
	AppId    string           `json:"appId"`
	Versions []*types.Version `json:"versions"`

	slotsLock sync.Mutex
	slots     map[int]*Slot `json:"slots"`

	Scontext *swancontext.SwanContext

	// app run with CurrentVersion config
	CurrentVersion *types.Version `json:"current_version"`
	// use when app updated, ProposedVersion can either be commit or revert
	ProposedVersion *types.Version `json:"proposed_version"`

	Mode AppMode `json:"mode"` // fixed or repliactes

	OfferAllocatorRef *OfferAllocator
	Created           time.Time
	Updated           time.Time

	State     string
	ClusterId string

	inTransaction bool
	touched       bool
}

func NewApp(version *types.Version,
	allocator *OfferAllocator,
	scontext *swancontext.SwanContext) (*App, error) {

	err := validateAndFormatVersion(version)
	if err != nil {
		return nil, err
	}

	app := &App{
		Versions:          []*types.Version{version},
		slots:             make(map[int]*Slot),
		CurrentVersion:    version,
		OfferAllocatorRef: allocator,
		AppId:             version.AppId,
		ClusterId:         mesos_connector.Instance().ClusterId,
		Scontext:          scontext,

		Created:       time.Now(),
		Updated:       time.Now(),
		inTransaction: false,
		touched:       true,
	}

	if version.Mode == "fixed" {
		app.Mode = APP_MODE_FIXED
	} else { // if no mode specified, default should be replicates
		app.Mode = APP_MODE_REPLICATES
	}
	version.ID = fmt.Sprintf("%d", time.Now().Unix())

	if err := WithConvertApp(context.TODO(), app, nil, persistentStore.CreateApp); err != nil {
		return nil, err
	}

	for i := 0; i < int(version.Instances); i++ {
		slot := NewSlot(app, version, i)
		app.SetSlot(i, slot)
		slot.DispatchNewTask(slot.Version)
	}

	app.SetState(APP_STATE_MARK_FOR_CREATING)

	app.create()

	return app, nil
}

// also need user pass ip here
func (app *App) ScaleUp(newInstances int, newIps []string) error {
	if !app.StateIs(APP_STATE_NORMAL) {
		return errors.New("app not in normal state")
	}

	if newInstances <= 0 {
		return errors.New("specify instances num want to increase")
	}

	if app.IsFixed() && len(newIps) != newInstances {
		return errors.New(fmt.Sprintf("please provide %d unique ip", newInstances))
	}

	app.BeginTx()
	defer app.Commit()

	app.CurrentVersion.Ip = append(app.CurrentVersion.Ip, newIps...)
	app.CurrentVersion.Instances += int32(newInstances)
	app.Updated = time.Now()

	app.SetState(APP_STATE_MARK_FOR_SCALE_UP)

	for i := newInstances; i > 0; i-- {
		slotIndex := int(app.CurrentVersion.Instances) - i
		slot := NewSlot(app, app.CurrentVersion, slotIndex)
		app.SetSlot(slotIndex, slot)
		slot.DispatchNewTask(slot.Version)
	}
	return nil
}

func (app *App) ScaleDown(removeInstances int) error {
	if !app.StateIs(APP_STATE_NORMAL) {
		return errors.New("app not in normal state")
	}

	if removeInstances >= int(app.CurrentVersion.Instances) {
		return errors.New(fmt.Sprintf("no more than %d instances can be shutdown", app.CurrentVersion.Instances))
	}

	app.BeginTx()
	defer app.Commit()

	app.CurrentVersion.Instances = int32(int(app.CurrentVersion.Instances) - removeInstances)
	app.Updated = time.Now()

	app.SetState(APP_STATE_MARK_FOR_SCALE_DOWN)

	for i := removeInstances; i > 0; i-- {
		slotIndex := int(app.CurrentVersion.Instances) + i - 1
		defer func(slotIndex int) {
			if slot, found := app.GetSlot(slotIndex); found {
				slot.Kill()
			}
		}(slotIndex)
	}

	return nil
}

// delete a application and all related objects: versions, tasks, slots, proxies, dns record
func (app *App) Delete() error {
	app.BeginTx()
	defer app.Commit()

	app.SetState(APP_STATE_MARK_FOR_DELETION)

	for _, slot := range app.slots {
		slot.Kill()
	}

	return nil
}

// update application by follower steps
// 1. check app state: if app state if not APP_STATE_NORMAL or app's propose version is not nil
//    we can not update app, because that means target app maybe is in updateing.
// 2. set the new version to the app's propose version.
// 3. persist app data, and set the app's state to APP_STATE_MARK_FOR_UPDATING
// 4. update slot version to propose version
// 5. after all slot version update success. put the current version to version history and set the
//    propose version as current version, set propose version to nil.
// 6. set app's state to APP_STATE_NORMAL.
func (app *App) Update(version *types.Version, store store.Store) error {
	if !app.StateIs(APP_STATE_NORMAL) || app.ProposedVersion != nil {
		return errors.New("app not in normal state")
	}

	if err := validateAndFormatVersion(version); err != nil {
		return err
	}

	if err := app.checkProposedVersionValid(version); err != nil {
		return err
	}

	app.BeginTx()
	defer app.Commit()

	if app.CurrentVersion == nil {
		return errors.New("update failed: current version was losted")
	}

	app.SetState(APP_STATE_MARK_FOR_UPDATING)

	version.ID = fmt.Sprintf("%d", time.Now().Unix())
	version.PerviousVersionID = app.CurrentVersion.ID
	app.ProposedVersion = version

	for i := 0; i < 1; i++ { // current we make first slot update
		if slot, found := app.GetSlot(i); found {
			slot.UpdateTask(app.ProposedVersion, true)
		}
	}

	return nil
}

func (app *App) ProceedingRollingUpdate(instances int) error {
	if app.ProposedVersion == nil {
		return errors.New("app not in rolling update state")
	}

	if instances < 1 {
		return errors.New("please specify how many instance want proceeding the update")
	}

	if (instances + app.RollingUpdateInstances()) > int(app.CurrentVersion.Instances) {
		return errors.New("update instances count exceed the maximum instances number")
	}

	app.BeginTx()
	defer app.Commit()

	for i := 0; i < instances; i++ {
		slotIndex := i + app.RollingUpdateInstances()
		defer func(slotIndex int) { // RollingUpdateInstances() has side effects in the loop
			if slot, found := app.GetSlot(slotIndex); found {
				slot.UpdateTask(app.ProposedVersion, true)
			}
		}(slotIndex)
	}

	return nil
}

func (app *App) CancelUpdate() error {
	if app.State != APP_STATE_MARK_FOR_UPDATING || app.ProposedVersion == nil {
		return errors.New("app not in updating state")
	}

	if app.CurrentVersion == nil {
		return errors.New("cancel update failed: current version was nil")
	}

	app.BeginTx()
	defer app.Commit()

	app.SetState(APP_STATE_MARK_FOR_CANCEL_UPDATE)

	for i := app.RollingUpdateInstances() - 1; i >= 0; i-- {
		if slot, found := app.GetSlot(i); found {
			slot.UpdateTask(app.CurrentVersion, true)
		}
	}

	return nil
}

func (app *App) IsReplicates() bool {
	return app.Mode == APP_MODE_REPLICATES
}

func (app *App) IsFixed() bool {
	return app.Mode == APP_MODE_FIXED
}

func (app *App) SetState(state string) {
	app.State = state
	app.Touch(false)
	logrus.Infof("app %s now has state %s", app.AppId, app.State)
}

func (app *App) StateIs(state string) bool {
	return app.State == state
}

func (app *App) RunningInstances() int {
	runningInstances := 0
	for _, slot := range app.slots {
		if slot.StateIs(SLOT_STATE_TASK_RUNNING) {
			runningInstances += 1
		}
	}

	return runningInstances
}

func (app *App) RollingUpdateInstances() int {
	rollingUpdateInstances := 0
	for _, slot := range app.slots {
		if slot.MarkForRollingUpdate() {
			rollingUpdateInstances += 1
		}
	}

	return rollingUpdateInstances
}

func (app *App) MarkForDeletionInstances() int {
	markForDeletionInstances := 0
	for _, slot := range app.slots {
		if slot.MarkForDeletion() {
			markForDeletionInstances += 1
		}
	}

	return markForDeletionInstances
}

func (app *App) CanBeCleanAfterDeletion() bool {
	return app.StateIs(APP_STATE_MARK_FOR_DELETION) && len(app.slots) == 0
}

func (app *App) RemoveSlot(index int) {
	app.slotsLock.Lock()
	defer app.slotsLock.Unlock()

	if slot, found := app.GetSlot(index); found {
		slot.Remove()
	}
	delete(app.slots, index)
	app.Touch(false)
}

func (app *App) GetSlot(index int) (*Slot, bool) {
	slot, ok := app.slots[index]
	return slot, ok
}

func (app *App) GetSlots() []*Slot {
	slots := make([]*Slot, 0)
	for _, v := range app.slots {
		slots = append(slots, v)
	}

	slotsById := SlotsById(slots)
	sort.Sort(slotsById)

	return slotsById
}

func (app *App) SetSlot(index int, slot *Slot) {
	app.slotsLock.Lock()
	defer app.slotsLock.Unlock()

	app.slots[index] = slot
	app.Touch(false)
}

func (app *App) Reevaluate() {
	switch app.State {
	case APP_STATE_NORMAL:
	case APP_STATE_MARK_FOR_DELETION:
	case APP_STATE_MARK_FOR_UPDATING:
		// when updating done
		if (app.RollingUpdateInstances() == int(app.CurrentVersion.Instances)) &&
			(app.RunningInstances() == int(app.CurrentVersion.Instances)) { // not perfect as when instances number increase, all instances running might be hard to acheive
			app.SetState(APP_STATE_NORMAL)

			app.CurrentVersion = app.ProposedVersion
			app.Versions = append(app.Versions, app.CurrentVersion)
			app.ProposedVersion = nil

			// special case, invoke low level storage directly to make version persisted
			WithConvertApp(context.TODO(), app, nil, persistentStore.CommitAppProposeVersion)

			for _, slot := range app.slots {
				slot.SetMarkForRollingUpdate(false)
			}
		}

	case APP_STATE_MARK_FOR_CANCEL_UPDATE:
		// when update cancelled
		if app.slots[0].Version == app.CurrentVersion && // until the first slot has updated to CurrentVersion
			app.RunningInstances() == int(app.CurrentVersion.Instances) { // not perfect as when instances number increase, all instances running might be hard to achieve
			app.SetState(APP_STATE_NORMAL)
			app.ProposedVersion = nil

			for _, slot := range app.slots {
				slot.SetMarkForRollingUpdate(false)
			}
		}

	case APP_STATE_MARK_FOR_CREATING:
		if app.RunningInstances() == int(app.CurrentVersion.Instances) {
			app.SetState(APP_STATE_NORMAL)
		}

	case APP_STATE_MARK_FOR_SCALE_UP:
		if app.StateIs(APP_STATE_MARK_FOR_SCALE_UP) && (app.RunningInstances() == int(app.CurrentVersion.Instances)) {
			app.SetState(APP_STATE_NORMAL)
		}

	case APP_STATE_MARK_FOR_SCALE_DOWN:
		if len(app.slots) == int(app.CurrentVersion.Instances) &&
			app.MarkForDeletionInstances() == 0 {
			app.SetState(APP_STATE_NORMAL)
		}

	default:
	}

	app.Touch(false)
}

func (app *App) EmitEvent(swanEvent *swanevent.Event) {
	app.Scontext.EventBus.EventChan <- swanEvent
}

// make sure proposed version is valid then applied it to field ProposedVersion
func (app *App) checkProposedVersionValid(version *types.Version) error {
	// mode can not change
	// runAs can not change
	// app instances should same as current instances
	return nil
}

func validateAndFormatVersion(version *types.Version) error {
	if version.Container == nil {
		return errors.New("swan only support mesos docker containerization, no container found")
	}

	if version.Container.Docker == nil {
		return errors.New("swan only support mesos docker containerization, no container found")
	}

	if len(version.Mode) == 0 {
		version.Mode = string(APP_MODE_REPLICATES)
	}

	if (version.Mode != string(APP_MODE_REPLICATES)) && (version.Mode != string(APP_MODE_FIXED)) {
		return errors.New(fmt.Sprintf("enrecognized app mode %s", version.Mode))
	}

	// validation for fixed mode application
	if version.Mode == string(APP_MODE_FIXED) {
		if len(version.Ip) != int(version.Instances) {
			return errors.New(fmt.Sprintf("should provide exactly %d ip for FIXED type app", version.Instances))
		}

		if len(version.Container.Docker.PortMappings) > 0 {
			return errors.New("fixed mode application doesn't support portmapping")
		}

		if len(version.HealthChecks) > 0 {
			return errors.New("fixed mode application doesn't health check")
		}

		if strings.ToLower(version.Container.Docker.Network) != SWAN_RESERVED_NETWORK {
			return errors.New("fixed mode app suppose the only network driver should be macvlan and name is swan")
		}
	}

	// validation for replicates mode app
	if version.Mode == string(APP_MODE_REPLICATES) {
		// the only network driver should be **bridge**
		if strings.ToLower(version.Container.Docker.Network) != "bridge" {
			return errors.New("replicates mode app suppose the only network driver should be bridge")
		}

		// portMapping.Name should be mandatory
		for _, portmapping := range version.Container.Docker.PortMappings {
			if strings.TrimSpace(portmapping.Name) == "" {
				return errors.New("each port mapping should have a uniquely identified name")
			}
		}

		portNames := make([]string, 0)
		for _, portmapping := range version.Container.Docker.PortMappings {
			portNames = append(portNames, portmapping.Name)
		}

		// portName should be unique
		if !utils.SliceUnique(portNames) {
			return errors.New("each port mapping should have a uniquely identified name")
		}

		// portName for health check should mandatory
		for _, hc := range version.HealthChecks {
			if strings.TrimSpace(hc.PortName) == "" {
				return errors.New("port name should not empty and match name in docker's PortMappings")
			}

			// portName should present in dockers' portMappings definition
			if !utils.SliceContains(portNames, hc.PortName) {
				return errors.New(fmt.Sprintf("no port name %s found in docker's PortMappings", hc.PortName))
			}

			if !utils.SliceContains([]string{"tcp", "http", "TCP", "HTTP"}, hc.Protocol) {
				return errors.New(fmt.Sprintf("doesn't recoginized protocol %s for health check", hc.Protocol))
			}

			if hc.Protocol == "http" || hc.Protocol == "HTTP" {
				if len(hc.Path) == 0 {
					return errors.New("no path provided for health check with HTTP protocol")
				}
			}
		}
	}

	return nil
}

// 1, remove app from persisted storage
// 2, other cleanup process
func (app *App) Remove() {
	app.remove()
}

// storage related
func (app *App) Touch(force bool) {
	if force { // force update the app
		app.update()
		return
	}

	if app.inTransaction {
		app.touched = true
		logrus.Infof("delay update action as current app in between tranaction")
	} else {
		app.update()
	}
}

func (app *App) BeginTx() {
	app.inTransaction = true
}

// here we persist the app anyway, no matter it touched or not
func (app *App) Commit() {
	app.inTransaction = false
	app.touched = false
	app.update()
}

func (app *App) update() {
	logrus.Debugf("update app %s", app.AppId)
	WithConvertApp(context.TODO(), app, nil, persistentStore.UpdateApp)
	app.touched = false
}

func (app *App) create() {
	logrus.Debugf("create app %s", app.AppId)
	WithConvertApp(context.TODO(), app, nil, persistentStore.CreateApp)
	app.touched = false
}

func (app *App) remove() {
	logrus.Debugf("remove app %s", app.AppId)
	WithConvertApp(context.TODO(), nil, nil, persistentStore.UpdateApp)
	app.touched = false
}
