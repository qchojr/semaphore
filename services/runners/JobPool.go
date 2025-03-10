//
// Runner's job pool. NOT SERVER!!!
// Runner gets jobs from the server and put them to this pool.
//

package runners

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	log "github.com/Sirupsen/logrus"
	"github.com/ansible-semaphore/semaphore/db"
	"github.com/ansible-semaphore/semaphore/lib"
	"github.com/ansible-semaphore/semaphore/services/tasks"
	"github.com/ansible-semaphore/semaphore/util"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"
)

type jobLogRecord struct {
	taskID int
	record LogRecord
}

type resourceLock struct {
	lock   bool
	holder *job
}

// job presents current job on semaphore server.
type job struct {
	username        string
	incomingVersion *string

	// job presents remote or local job information
	job             *tasks.LocalJob
	status          db.TaskStatus
	args            []string
	environmentVars []string
}

type RunnerConfig struct {
	RunnerID int    `json:"runner_id"`
	Token    string `json:"token"`
}

type JobData struct {
	Username        string
	IncomingVersion *string
	Task            db.Task        `json:"task" binding:"required"`
	Template        db.Template    `json:"template" binding:"required"`
	Inventory       db.Inventory   `json:"inventory" binding:"required"`
	Repository      db.Repository  `json:"repository" binding:"required"`
	Environment     db.Environment `json:"environment" binding:"required"`
}

type RunnerState struct {
	CurrentJobs []JobState
	NewJobs     []JobData            `json:"new_jobs" binding:"required"`
	AccessKeys  map[int]db.AccessKey `json:"access_keys" binding:"required"`
}

type JobState struct {
	ID     int           `json:"id" binding:"required"`
	Status db.TaskStatus `json:"status" binding:"required"`
}

type LogRecord struct {
	Time    time.Time `json:"time" binding:"required"`
	Message string    `json:"message" binding:"required"`
}

type RunnerProgress struct {
	Jobs []JobProgress
}

type JobProgress struct {
	ID         int
	Status     db.TaskStatus
	LogRecords []LogRecord
}

type runningJob struct {
	status     db.TaskStatus
	logRecords []LogRecord
	job        *tasks.LocalJob
}

type JobPool struct {
	// logger channel used to putting log records to database.
	logger chan jobLogRecord

	// register channel used to put tasks to queue.
	register chan *job

	resourceLocker chan *resourceLock

	runningJobs map[int]*runningJob

	queue []*job

	config *RunnerConfig
}

type RunnerRegistration struct {
	RegistrationToken string `json:"registration_token" binding:"required"`
}

func (p *runningJob) Log2(msg string, now time.Time) {
	p.logRecords = append(p.logRecords, LogRecord{Time: now, Message: msg})
}

func (p *JobPool) hasRunningJobs() bool {
	for _, j := range p.runningJobs {
		if !j.status.IsFinished() {
			return true
		}
	}

	return false
}

func (p *runningJob) Log(msg string) {
	p.Log2(msg, time.Now())
}

func (p *runningJob) SetStatus(status db.TaskStatus) {
	p.status = status
}

func (p *runningJob) LogCmd(cmd *exec.Cmd) {
	stderr, _ := cmd.StderrPipe()
	stdout, _ := cmd.StdoutPipe()

	go p.logPipe(bufio.NewReader(stderr))
	go p.logPipe(bufio.NewReader(stdout))
}

func (p *runningJob) logPipe(reader *bufio.Reader) {

	line, err := tasks.Readln(reader)
	for err == nil {
		p.Log(line)
		line, err = tasks.Readln(reader)
	}

	if err != nil && err.Error() != "EOF" {
		//don't panic on these errors, sometimes it throws not dangerous "read |0: file already closed" error
		util.LogWarningWithFields(err, log.Fields{"error": "Failed to read TaskRunner output"})
	}

}

func (p *JobPool) Run() {
	queueTicker := time.NewTicker(5 * time.Second)
	requestTimer := time.NewTicker(1 * time.Second)
	p.runningJobs = make(map[int]*runningJob)

	defer func() {
		queueTicker.Stop()
	}()

	for {
		select {
		//case j := <-p.register: // new task created by API or schedule
		//	p.queue = append(p.queue, j)

		case <-queueTicker.C: // timer 5 seconds: get task from queue and run it
			if len(p.queue) == 0 {
				break
			}

			t := p.queue[0]
			if t.status == db.TaskFailStatus {
				//delete failed TaskRunner from queue
				p.queue = p.queue[1:]
				log.Info("Task " + strconv.Itoa(t.job.Task.ID) + " removed from queue")
				break
			}

			//log.Info("Set resource locker with TaskRunner " + strconv.Itoa(t.id))
			//p.resourceLocker <- &resourceLock{lock: true, holder: t}

			p.runningJobs[t.job.Task.ID] = &runningJob{
				job: t.job,
			}
			t.job.Logger = p.runningJobs[t.job.Task.ID]
			t.job.Playbook.Logger = t.job.Logger

			go func(runningJob *runningJob) {
				runningJob.SetStatus(db.TaskRunningStatus)

				err := runningJob.job.Run(t.username, t.incomingVersion)

				if runningJob.status.IsFinished() {
					return
				}

				if err != nil {
					if runningJob.status == db.TaskStoppingStatus {
						runningJob.SetStatus(db.TaskStoppedStatus)
					} else {
						runningJob.SetStatus(db.TaskFailStatus)
					}
				} else {
					runningJob.SetStatus(db.TaskSuccessStatus)
				}
			}(p.runningJobs[t.job.Task.ID])

			p.queue = p.queue[1:]
			log.Info("Task " + strconv.Itoa(t.job.Task.ID) + " removed from queue")

		case <-requestTimer.C:

			go p.sendProgress()

			if util.Config.Runner.OneOff && len(p.runningJobs) > 0 && !p.hasRunningJobs() {
				os.Exit(0)
			}

			go p.checkNewJobs()

		}
	}
}

func (p *JobPool) sendProgress() {

	if !p.tryRegisterRunner() {
		return
	}

	client := &http.Client{}

	url := util.Config.Runner.ApiURL + "/runners/" + strconv.Itoa(p.config.RunnerID)

	body := RunnerProgress{
		Jobs: nil,
	}

	for id, j := range p.runningJobs {
		body.Jobs = append(body.Jobs, JobProgress{
			ID:         id,
			LogRecords: j.logRecords,
			Status:     j.status,
		})

		j.logRecords = make([]LogRecord, 0)
	}

	jsonBytes, err := json.Marshal(body)

	req, err := http.NewRequest("PUT", url, bytes.NewBuffer(jsonBytes))
	if err != nil {
		fmt.Println("Error creating request:", err)
		return
	}

	resp, err := client.Do(req)
	if err != nil {
		fmt.Println("Error making request:", err)
		return
	}

	defer resp.Body.Close()
}

func (p *JobPool) tryRegisterRunner() bool {
	if p.config != nil {
		return true
	}

	_, err := os.Stat(util.Config.Runner.ConfigFile)

	if err == nil {
		configBytes, err2 := os.ReadFile(util.Config.Runner.ConfigFile)

		if err2 != nil {
			panic(err2)
		}

		var config RunnerConfig

		err2 = json.Unmarshal(configBytes, &config)

		if err2 != nil {
			panic(err2)
		}

		p.config = &config

		return true
	}

	if !os.IsNotExist(err) {
		panic(err)
	}

	if util.Config.Runner.RegistrationToken == "" {
		panic("registration token cannot be empty")
	}

	client := &http.Client{}

	url := util.Config.Runner.ApiURL + "/runners"

	jsonBytes, err := json.Marshal(RunnerRegistration{
		RegistrationToken: util.Config.Runner.RegistrationToken,
	})

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonBytes))
	if err != nil {
		fmt.Println("Error creating request:", err)
		return false
	}

	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != 200 {
		fmt.Println("Error making request:", err)
		return false
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Println("Error reading response body:", err)
		return false
	}

	var config RunnerConfig
	err = json.Unmarshal(body, &config)
	if err != nil {
		fmt.Println("Error parsing JSON:", err)
		return false
	}

	configBytes, err := json.Marshal(config)

	if err != nil {
		panic("cannot save runner config")
	}

	err = os.WriteFile(util.Config.Runner.ConfigFile, configBytes, 0644)

	p.config = &config

	defer resp.Body.Close()

	return true
}

// checkNewJobs tries to find runner to queued jobs
func (p *JobPool) checkNewJobs() {

	if !p.tryRegisterRunner() {
		return
	}

	client := &http.Client{}

	url := util.Config.Runner.ApiURL + "/runners/" + strconv.Itoa(p.config.RunnerID)

	req, err := http.NewRequest("GET", url, nil)

	if err != nil {
		fmt.Println("Error creating request:", err)
		return
	}

	resp, err := client.Do(req)
	if err != nil {
		fmt.Println("Error making request:", err)
		return
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Println("Error reading response body:", err)
		return
	}

	var response RunnerState
	err = json.Unmarshal(body, &response)
	if err != nil {
		fmt.Println("Error parsing JSON:", err)
		return
	}

	for _, currJob := range response.CurrentJobs {
		runJob, exists := p.runningJobs[currJob.ID]

		if !exists {
			continue
		}

		runJob.SetStatus(currJob.Status)

		if runJob.status == db.TaskStoppingStatus || runJob.status == db.TaskStoppedStatus {
			p.runningJobs[currJob.ID].job.Kill()
		}
	}

	if util.Config.Runner.OneOff {
		if len(p.queue) > 0 || len(p.runningJobs) > 0 {
			return
		}
	}

	for _, newJob := range response.NewJobs {
		if _, exists := p.runningJobs[newJob.Task.ID]; exists {
			continue
		}

		taskRunner := job{
			username:        newJob.Username,
			incomingVersion: newJob.IncomingVersion,

			job: &tasks.LocalJob{
				Task:        newJob.Task,
				Template:    newJob.Template,
				Inventory:   newJob.Inventory,
				Repository:  newJob.Repository,
				Environment: newJob.Environment,
				Playbook: &lib.AnsiblePlaybook{
					TemplateID: newJob.Template.ID,
					Repository: newJob.Repository,
				},
			},
		}

		taskRunner.job.Repository.SSHKey = response.AccessKeys[taskRunner.job.Repository.SSHKeyID]

		if taskRunner.job.Inventory.SSHKeyID != nil {
			taskRunner.job.Inventory.SSHKey = response.AccessKeys[*taskRunner.job.Inventory.SSHKeyID]
		}

		if taskRunner.job.Inventory.BecomeKeyID != nil {
			taskRunner.job.Inventory.BecomeKey = response.AccessKeys[*taskRunner.job.Inventory.BecomeKeyID]
		}

		if taskRunner.job.Template.VaultKeyID != nil {
			taskRunner.job.Template.VaultKey = response.AccessKeys[*taskRunner.job.Template.VaultKeyID]
		}

		p.queue = append(p.queue, &taskRunner)
	}
}
