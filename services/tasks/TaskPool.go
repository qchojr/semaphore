package tasks

import (
	"github.com/ansible-semaphore/semaphore/db"
	"github.com/ansible-semaphore/semaphore/lib"
	"regexp"
	"strconv"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/ansible-semaphore/semaphore/util"
)

type logRecord struct {
	task   *TaskRunner
	output string
	time   time.Time
}

type resourceLock struct {
	lock   bool
	holder *TaskRunner
}

type TaskPool struct {
	// queue contains list of tasks in status TaskWaitingStatus.
	queue []*TaskRunner

	// register channel used to put tasks to queue.
	register chan *TaskRunner

	activeProj map[int]map[int]*TaskRunner

	// runningTasks contains tasks with status TaskRunningStatus.
	runningTasks map[int]*TaskRunner

	// logger channel used to putting log records to database.
	logger chan logRecord

	store db.Store

	resourceLocker chan *resourceLock
}

func (p *TaskPool) GetRunningTasks() (res []*TaskRunner) {
	for _, task := range p.runningTasks {
		res = append(res, task)
	}
	return
}

func (p *TaskPool) GetTask(id int) (task *TaskRunner) {

	for _, t := range p.queue {
		if t.Task.ID == id {
			task = t
			break
		}
	}

	if task == nil {
		for _, t := range p.runningTasks {
			if t.Task.ID == id {
				task = t
				break
			}
		}
	}

	return
}

// nolint: gocyclo
func (p *TaskPool) Run() {
	ticker := time.NewTicker(5 * time.Second)

	defer func() {
		close(p.resourceLocker)
		ticker.Stop()
	}()

	// Lock or unlock resources when running a TaskRunner
	go func(locker <-chan *resourceLock) {
		for l := range locker {
			t := l.holder

			if l.lock {
				if p.blocks(t) {
					panic("Trying to lock an already locked resource!")
				}

				projTasks, ok := p.activeProj[t.Task.ProjectID]
				if !ok {
					projTasks = make(map[int]*TaskRunner)
					p.activeProj[t.Task.ProjectID] = projTasks
				}
				projTasks[t.Task.ID] = t
				p.runningTasks[t.Task.ID] = t
				continue
			}

			if p.activeProj[t.Task.ProjectID] != nil && p.activeProj[t.Task.ProjectID][t.Task.ID] != nil {
				delete(p.activeProj[t.Task.ProjectID], t.Task.ID)
				if len(p.activeProj[t.Task.ProjectID]) == 0 {
					delete(p.activeProj, t.Task.ProjectID)
				}
			}

			delete(p.runningTasks, t.Task.ID)
		}
	}(p.resourceLocker)

	for {
		select {
		case record := <-p.logger: // new log message which should be put to database
			db.StoreSession(p.store, "logger", func() {
				_, err := p.store.CreateTaskOutput(db.TaskOutput{
					TaskID: record.task.Task.ID,
					Output: record.output,
					Time:   record.time,
				})
				if err != nil {
					log.Error(err)
				}
			})

		case task := <-p.register: // new task created by API or schedule

			db.StoreSession(p.store, "new task", func() {
				p.queue = append(p.queue, task)
				log.Debug(task)
				msg := "Task " + strconv.Itoa(task.Task.ID) + " added to queue"
				task.Log(msg)
				log.Info(msg)
				task.saveStatus()
			})

		case <-ticker.C: // timer 5 seconds
			if len(p.queue) == 0 {
				break
			}

			//get TaskRunner from top of queue
			t := p.queue[0]
			if t.Task.Status == db.TaskFailStatus {
				//delete failed TaskRunner from queue
				p.queue = p.queue[1:]
				log.Info("Task " + strconv.Itoa(t.Task.ID) + " removed from queue")
				break
			}

			if p.blocks(t) {
				//move blocked TaskRunner to end of queue
				p.queue = append(p.queue[1:], t)
				break
			}

			log.Info("Set resource locker with TaskRunner " + strconv.Itoa(t.Task.ID))
			p.resourceLocker <- &resourceLock{lock: true, holder: t}

			go t.run()

			p.queue = p.queue[1:]
			log.Info("Task " + strconv.Itoa(t.Task.ID) + " removed from queue")
		}
	}
}

func (p *TaskPool) blocks(t *TaskRunner) bool {

	if len(p.runningTasks) >= util.Config.MaxParallelTasks {
		return true
	}

	if p.activeProj[t.Task.ProjectID] == nil || len(p.activeProj[t.Task.ProjectID]) == 0 {
		return false
	}

	for _, r := range p.activeProj[t.Task.ProjectID] {
		if r.Template.ID == t.Task.TemplateID {
			return true
		}
	}

	proj, err := p.store.GetProject(t.Task.ProjectID)

	if err != nil {
		log.Error(err)
		return false
	}

	return proj.MaxParallelTasks > 0 && len(p.activeProj[t.Task.ProjectID]) >= proj.MaxParallelTasks
}

func CreateTaskPool(store db.Store) TaskPool {
	return TaskPool{
		queue:          make([]*TaskRunner, 0), // queue of waiting tasks
		register:       make(chan *TaskRunner), // add TaskRunner to queue
		activeProj:     make(map[int]map[int]*TaskRunner),
		runningTasks:   make(map[int]*TaskRunner),   // working tasks
		logger:         make(chan logRecord, 10000), // store log records to database
		store:          store,
		resourceLocker: make(chan *resourceLock),
	}
}

func (p *TaskPool) StopTask(targetTask db.Task, forceStop bool) error {
	tsk := p.GetTask(targetTask.ID)
	if tsk == nil { // task not active, but exists in database
		tsk = &TaskRunner{
			Task: targetTask,
			pool: p,
		}
		err := tsk.populateDetails()
		if err != nil {
			return err
		}
		tsk.SetStatus(db.TaskStoppedStatus)
		tsk.createTaskEvent()
	} else {
		status := tsk.Task.Status

		if forceStop {
			tsk.SetStatus(db.TaskStoppedStatus)
		} else {
			tsk.SetStatus(db.TaskStoppingStatus)
		}

		if status == db.TaskRunningStatus {
			tsk.kill()
		}
	}

	return nil
}

func getNextBuildVersion(startVersion string, currentVersion string) string {
	re := regexp.MustCompile(`^(.*[^\d])?(\d+)([^\d].*)?$`)
	m := re.FindStringSubmatch(startVersion)

	if m == nil {
		return startVersion
	}

	var prefix, suffix, body string

	switch len(m) - 1 {
	case 3:
		prefix = m[1]
		body = m[2]
		suffix = m[3]
	case 2:
		if _, err := strconv.Atoi(m[1]); err == nil {
			body = m[1]
			suffix = m[2]
		} else {
			prefix = m[1]
			body = m[2]
		}
	case 1:
		body = m[1]
	default:
		return startVersion
	}

	if !strings.HasPrefix(currentVersion, prefix) ||
		!strings.HasSuffix(currentVersion, suffix) {
		return startVersion
	}

	curr, err := strconv.Atoi(currentVersion[len(prefix) : len(currentVersion)-len(suffix)])
	if err != nil {
		return startVersion
	}

	start, err := strconv.Atoi(body)
	if err != nil {
		panic(err)
	}

	var newVer int
	if start > curr {
		newVer = start
	} else {
		newVer = curr + 1
	}

	return prefix + strconv.Itoa(newVer) + suffix
}

func (p *TaskPool) AddTask(taskObj db.Task, userID *int, projectID int) (newTask db.Task, err error) {
	taskObj.Created = time.Now()
	taskObj.Status = db.TaskWaitingStatus
	taskObj.UserID = userID
	taskObj.ProjectID = projectID

	tpl, err := p.store.GetTemplate(projectID, taskObj.TemplateID)
	if err != nil {
		return
	}

	err = taskObj.ValidateNewTask(tpl)
	if err != nil {
		return
	}

	if tpl.Type == db.TemplateBuild { // get next version for TaskRunner if it is a Build
		var builds []db.TaskWithTpl
		builds, err = p.store.GetTemplateTasks(tpl.ProjectID, tpl.ID, db.RetrieveQueryParams{Count: 1})
		if err != nil {
			return
		}
		if len(builds) == 0 || builds[0].Version == nil {
			taskObj.Version = tpl.StartVersion
		} else {
			v := getNextBuildVersion(*tpl.StartVersion, *builds[0].Version)
			taskObj.Version = &v
		}
	}

	newTask, err = p.store.CreateTask(taskObj)
	if err != nil {
		return
	}

	taskRunner := TaskRunner{
		Task: newTask,
		pool: p,
	}

	err = taskRunner.populateDetails()
	if err != nil {
		taskRunner.Log("Error: " + err.Error())
		taskRunner.SetStatus(db.TaskFailStatus)
		return
	}

	var job Job

	if util.Config.UseRemoteRunner {
		job = &RemoteJob{
			Task:        taskRunner.Task,
			Template:    taskRunner.Template,
			Inventory:   taskRunner.Inventory,
			Repository:  taskRunner.Repository,
			Environment: taskRunner.Environment,
			Logger:      &taskRunner,
			Playbook: &lib.AnsiblePlaybook{
				Logger:     &taskRunner,
				TemplateID: taskRunner.Template.ID,
				Repository: taskRunner.Repository,
			},
			taskPool: p,
		}
	} else {
		job = &LocalJob{
			Task:        taskRunner.Task,
			Template:    taskRunner.Template,
			Inventory:   taskRunner.Inventory,
			Repository:  taskRunner.Repository,
			Environment: taskRunner.Environment,
			Logger:      &taskRunner,
			Playbook: &lib.AnsiblePlaybook{
				Logger:     &taskRunner,
				TemplateID: taskRunner.Template.ID,
				Repository: taskRunner.Repository,
			},
		}
	}

	if err != nil {
		return
	}

	taskRunner.job = job

	p.register <- &taskRunner

	objType := db.EventTask
	desc := "Task ID " + strconv.Itoa(newTask.ID) + " queued for running"
	_, err = p.store.CreateEvent(db.Event{
		UserID:      userID,
		ProjectID:   &projectID,
		ObjectType:  &objType,
		ObjectID:    &newTask.ID,
		Description: &desc,
	})

	return
}
