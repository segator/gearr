package task

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"gearr/helper"
	"gearr/helper/command"
	"gearr/model"
	"hash"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/avast/retry-go"
	log "github.com/sirupsen/logrus"
	"gopkg.in/vansante/go-ffprobe.v2"
)

const RESET_LINE = "\r\033[K"

var ffmpegSpeedRegex = regexp.MustCompile(`speed=(\d*\.?\d+)x`)
var ErrorJobNotFound = errors.New("job Not found")

type FFMPEGProgress struct {
	duration int
	speed    float64
	percent  float64
}

type EncodeWorker struct {
	model.Manager
	name            string
	ctx             context.Context
	cancelContext   context.CancelFunc
	maxPrefetchJobs uint32
	prefetchJobs    uint32
	downloadChan    chan *model.WorkTaskEncode
	encodeChan      chan *model.WorkTaskEncode
	uploadChan      chan *model.WorkTaskEncode
	workerConfig    Config
	tempPath        string
	wg              sync.WaitGroup
	mu              sync.RWMutex
	terminal        *ConsoleWorkerPrinter
	ctxStopQueues   context.Context
	stopQueues      context.CancelFunc
}

func ensureDirectoryExists(path string) {
	os.MkdirAll(path, os.ModePerm)
}

func NewEncodeWorker(ctx context.Context, workerConfig Config, workerName string, printer *ConsoleWorkerPrinter) *EncodeWorker {
	newCtx, cancel := context.WithCancel(ctx)
	ctxStopQueues, stopQueues := context.WithCancel(ctx)
	tempPath := filepath.Join(workerConfig.TemporalPath, fmt.Sprintf("worker-%s", workerName))

	ensureDirectoryExists(tempPath)

	return &EncodeWorker{
		name:            workerName,
		ctx:             newCtx,
		ctxStopQueues:   ctxStopQueues,
		stopQueues:      stopQueues,
		wg:              sync.WaitGroup{},
		cancelContext:   cancel,
		workerConfig:    workerConfig,
		downloadChan:    make(chan *model.WorkTaskEncode, 100),
		encodeChan:      make(chan *model.WorkTaskEncode, 100),
		uploadChan:      make(chan *model.WorkTaskEncode, 100),
		tempPath:        tempPath,
		terminal:        printer,
		maxPrefetchJobs: uint32(workerConfig.MaxPrefetchJobs),
		prefetchJobs:    0,
	}
}

func durToSec(dur string) (sec int) {
	durAry := strings.Split(dur, ":")
	if len(durAry) == 3 {
		hr, _ := strconv.Atoi(durAry[0])
		sec = hr * (60 * 60)
		min, _ := strconv.Atoi(durAry[1])
		sec += min * 60
		second, _ := strconv.Atoi(durAry[2])
		sec += second
	}
	return
}

func getSpeed(res string) float64 {
	rs := ffmpegSpeedRegex.FindStringSubmatch(res)
	if len(rs) == 0 {
		return -1
	}
	speed, err := strconv.ParseFloat(rs[1], 64)
	if err != nil {
		return -1
	}
	return speed
}

func getDuration(res string) int {
	i := strings.Index(res, "time=")
	if i >= 0 {
		time := res[i+5:]
		if len(time) > 8 {
			time = time[0:8]
			sec := durToSec(time)
			return sec
		}
	}
	return -1
}

func (E *EncodeWorker) Initialize() {
	E.resumeJobs()
	go E.terminal.Render()
	go E.downloadQueue()

	for i := 0; i < E.workerConfig.EncodeJobs; i++ {
		go E.uploadQueue()
		go E.encodeQueue()
	}

}

func (E *EncodeWorker) resumeJobs() {
	err := filepath.Walk(E.tempPath, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}

		taskEncode := E.readTaskStatusFromDiskByPath(path)

		switch {
		case taskEncode.LastState.IsDownloading():
			E.AddDownloadJob(taskEncode.Task)
		case taskEncode.LastState.IsEncoding():
			atomic.AddUint32(&E.prefetchJobs, 1)
			t := E.terminal.AddTask(fmt.Sprintf("cached: %s", taskEncode.Task.TaskEncode.Id.String()), DownloadJobStepType)
			t.Done()
			E.encodeChan <- taskEncode.Task
		case taskEncode.LastState.IsUploading():
			t := E.terminal.AddTask(fmt.Sprintf("cached: %s", taskEncode.Task.TaskEncode.Id.String()), EncodeJobStepType)
			t.Done()
			E.uploadChan <- taskEncode.Task
		}

		return nil
	})

	if err != nil {
		panic(err)
	}
}
func (J *EncodeWorker) IsTypeAccepted(jobType string) bool {
	return jobType == string(model.EncodeJobType)
}

func (J *EncodeWorker) AcceptJobs() bool {
	now := time.Now()
	if J.workerConfig.Paused {
		return false
	}
	if J.workerConfig.HaveSetPeriodTime() {
		startAfter := time.Date(now.Year(), now.Month(), now.Day(), J.workerConfig.StartAfter.Hour, J.workerConfig.StartAfter.Minute, 0, 0, now.Location())
		stopAfter := time.Date(now.Year(), now.Month(), now.Day(), J.workerConfig.StopAfter.Hour, J.workerConfig.StopAfter.Minute, 0, 0, now.Location())
		return now.After(startAfter) && now.Before(stopAfter)
	}
	return J.PrefetchJobs() < uint32(J.workerConfig.MaxPrefetchJobs)
}

func (J *EncodeWorker) downloadFile(job *model.WorkTaskEncode, track *TaskTracks) error {
	err := retry.Do(func() error {
		track.UpdateValue(0)
		resp, err := http.Get(job.TaskEncode.DownloadURL)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusNotFound {
			return ErrorJobNotFound
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("non-200 response in download code %d", resp.StatusCode)
		}

		size, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
		if err != nil {
			return err
		}
		track.SetTotal(size)

		_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
		if err != nil {
			return err
		}

		job.SourceFilePath = filepath.Join(job.WorkDir, fmt.Sprintf("%s%s", job.TaskEncode.Id.String(), filepath.Ext(params["filename"])))
		downloadFile, err := os.Create(job.SourceFilePath)
		if err != nil {
			return err
		}
		defer downloadFile.Close()

		reader := NewProgressTrackStream(track, resp.Body)
		_, err = io.Copy(downloadFile, reader)
		if err != nil {
			return err
		}

		sha256String := hex.EncodeToString(reader.SumSha())
		bodyString, checksumErr := J.calculateChecksum(job.TaskEncode.ChecksumURL)
		if checksumErr != nil {
			return checksumErr
		}

		if sha256String != bodyString {
			return fmt.Errorf("checksum error on download source:%s downloaded:%s", bodyString, sha256String)
		}

		track.UpdateValue(size)
		return nil
	}, retry.Delay(time.Second*5),
		retry.Attempts(180), // 15 min
		retry.LastErrorOnly(true),
		retry.OnRetry(func(n uint, err error) {
			J.terminal.Error("error on downloading job %s", err.Error())
		}),
		retry.RetryIf(func(err error) bool {
			return !(errors.Is(err, context.Canceled) || errors.Is(err, ErrorJobNotFound))
		}))

	return err
}

func (J *EncodeWorker) calculateChecksum(checksumURL string) (string, error) {
	var bodyString string

	err := retry.Do(func() error {
		respSha256, err := http.Get(checksumURL)
		if err != nil {
			return err
		}
		defer respSha256.Body.Close()

		if respSha256.StatusCode != http.StatusOK {
			return fmt.Errorf("non 200 response in sha265 code %d", respSha256.StatusCode)
		}

		bodyBytes, err := io.ReadAll(respSha256.Body)
		if err != nil {
			return err
		}
		bodyString = string(bodyBytes)
		return nil
	}, retry.Delay(time.Second*5),
		retry.Attempts(10),
		retry.LastErrorOnly(true),
		retry.OnRetry(func(n uint, err error) {
			J.terminal.Error("error %s on calculate checksum of downloaded job %s", err.Error(), checksumURL)
		}),
		retry.RetryIf(func(err error) bool {
			return !errors.Is(err, context.Canceled)
		}))

	if err != nil {
		return "", err
	}

	return bodyString, nil
}

func (J *EncodeWorker) getVideoParameters(inputFile string) (data *ffprobe.ProbeData, size int64, err error) {
	fileReader, err := os.Open(inputFile)
	if err != nil {
		return nil, -1, fmt.Errorf("error opening file %s: %v", inputFile, err)
	}
	defer fileReader.Close()

	stat, err := fileReader.Stat()
	if err != nil {
		return nil, 0, err
	}

	data, err = ffprobe.ProbeReader(J.ctx, fileReader)
	if err != nil {
		return nil, 0, fmt.Errorf("error getting data: %v", err)
	}

	return data, stat.Size(), nil
}

func FFProbeFrameRate(FFProbeFrameRate string) (frameRate int, err error) {
	avgFrameSpl := strings.Split(FFProbeFrameRate, "/")
	if len(avgFrameSpl) != 2 {
		return 0, errors.New("invalid format")
	}

	frameRatio, err := strconv.Atoi(avgFrameSpl[0])
	if err != nil {
		return 0, err
	}

	rate, err := strconv.Atoi(avgFrameSpl[1])
	if err != nil {
		return 0, err
	}

	return frameRatio / rate, nil
}

func (J *EncodeWorker) clearData(data *ffprobe.ProbeData) (*ContainerData, error) {
	container := &ContainerData{}

	videoStream := data.StreamType(ffprobe.StreamVideo)[0]
	frameRate, err := FFProbeFrameRate(videoStream.AvgFrameRate)
	if err != nil {
		frameRate = 24
	}

	container.Video = &Video{
		Id:        uint8(videoStream.Index),
		Duration:  data.Format.Duration(),
		FrameRate: frameRate,
	}

	betterAudioStreamPerLanguage := make(map[string]*Audio)

	for _, stream := range data.StreamType(ffprobe.StreamAudio) {
		if stream.BitRate == "" {
			stream.BitRate = "0"
		}

		bitRateInt, err := strconv.ParseUint(stream.BitRate, 10, 32)
		if err != nil {
			return nil, err
		}

		newAudio := &Audio{
			Id:             uint8(stream.Index),
			Language:       stream.Tags.Language,
			Channels:       stream.ChannelLayout,
			ChannelsNumber: uint8(stream.Channels),
			ChannelLayour:  stream.ChannelLayout,
			Default:        stream.Disposition.Default == 1,
			Bitrate:        uint(bitRateInt),
			Title:          stream.Tags.Title,
		}

		betterAudio := betterAudioStreamPerLanguage[newAudio.Language]

		if betterAudio != nil && (newAudio.ChannelsNumber > betterAudio.ChannelsNumber || (newAudio.ChannelsNumber == betterAudio.ChannelsNumber && newAudio.Bitrate > betterAudio.Bitrate)) {
			betterAudioStreamPerLanguage[newAudio.Language] = newAudio
		} else if betterAudio == nil {
			betterAudioStreamPerLanguage[newAudio.Language] = newAudio
		}
	}

	for _, audioStream := range betterAudioStreamPerLanguage {
		container.Audios = append(container.Audios, audioStream)
	}

	betterSubtitleStreamPerLanguage := make(map[string]*Subtitle)

	for _, stream := range data.StreamType(ffprobe.StreamSubtitle) {
		newSubtitle := &Subtitle{
			Id:       uint8(stream.Index),
			Language: stream.Tags.Language,
			Forced:   stream.Disposition.Forced == 1,
			Comment:  stream.Disposition.Comment == 1,
			Format:   stream.CodecName,
			Title:    stream.Tags.Title,
		}

		if newSubtitle.Forced || newSubtitle.Comment {
			container.Subtitle = append(container.Subtitle, newSubtitle)
			continue
		}

		betterSubtitle := betterSubtitleStreamPerLanguage[newSubtitle.Language]

		if betterSubtitle == nil {
			betterSubtitleStreamPerLanguage[newSubtitle.Language] = newSubtitle
		} else {
			container.Subtitle = append(container.Subtitle, newSubtitle)
		}
	}

	for _, value := range betterSubtitleStreamPerLanguage {
		container.Subtitle = append(container.Subtitle, value)
	}

	return container, nil
}

func (J *EncodeWorker) FFMPEG(job *model.WorkTaskEncode, videoContainer *ContainerData, ffmpegProgressChan chan<- FFMPEGProgress) error {
	ffmpeg := &FFMPEGGenerator{}
	ffmpeg.setInputFilters(videoContainer, job.SourceFilePath, job.WorkDir)
	ffmpeg.setVideoFilters(videoContainer)
	ffmpeg.setAudioFilters(videoContainer)
	ffmpeg.setSubtFilters(videoContainer)
	ffmpeg.setMetadata(videoContainer)

	ffmpegErrLog := ""
	ffmpegOutLog := ""

	sendObj := FFMPEGProgress{
		duration: -1,
		speed:    -1,
	}

	isClosed := false
	defer func() {
		// close(ffmpegProgressChan)
		isClosed = true
	}()

	checkPercentageFFMPEG := func(buffer []byte, exit bool) {
		stringedBuffer := string(buffer)
		ffmpegErrLog += stringedBuffer

		duration := getDuration(stringedBuffer)
		if duration != -1 {
			sendObj.duration = duration
			sendObj.percent = float64(duration*100) / videoContainer.Video.Duration.Seconds()
		}

		speed := getSpeed(stringedBuffer)
		if speed != -1 {
			sendObj.speed = speed
		}

		if sendObj.speed != -1 && sendObj.duration != -1 && !isClosed {
			ffmpegProgressChan <- sendObj
			sendObj.duration = -1
			sendObj.speed = -1
		}
	}

	stdoutFFMPEG := func(buffer []byte, exit bool) {
		ffmpegOutLog += string(buffer)
	}

	sourceFileName := filepath.Base(job.SourceFilePath)
	encodedFilePath := fmt.Sprintf("%s-encoded.%s", strings.TrimSuffix(sourceFileName, filepath.Ext(sourceFileName)), "mkv")
	job.TargetFilePath = filepath.Join(job.WorkDir, encodedFilePath)

	ffmpegArguments := ffmpeg.buildArguments(uint8(J.workerConfig.Threads), job.TargetFilePath)
	J.terminal.Cmd("FFMPEG Command:%s %s", helper.GetFFmpegPath(), ffmpegArguments)

	ffmpegCommand := command.NewCommandByString(helper.GetFFmpegPath(), ffmpegArguments).
		SetWorkDir(job.WorkDir).
		SetStdoutFunc(stdoutFFMPEG).
		SetStderrFunc(checkPercentageFFMPEG)

	if runtime.GOOS == "linux" {
		ffmpegCommand.AddEnv(fmt.Sprintf("LD_LIBRARY_PATH=%s", filepath.Dir(helper.GetFFmpegPath())))
	}

	exitCode, err := ffmpegCommand.RunWithContext(J.ctx)
	if err != nil {
		return fmt.Errorf("%w: stderr:%s stdout:%s", err, ffmpegErrLog, ffmpegOutLog)
	}

	if exitCode != 0 {
		return fmt.Errorf("exit code %d: stderr:%s stdout:%s", exitCode, ffmpegErrLog, ffmpegOutLog)
	}

	return nil
}

type ProgressTrackReader struct {
	taskTracker *TaskTracks
	io.ReadCloser
	sha hash.Hash
}

func NewProgressTrackStream(track *TaskTracks, reader io.ReadCloser) *ProgressTrackReader {
	return &ProgressTrackReader{
		taskTracker: track,
		ReadCloser:  reader,
		sha:         sha256.New(),
	}
}

func (P *ProgressTrackReader) Read(p []byte) (n int, err error) {
	n, err = P.ReadCloser.Read(p)
	P.taskTracker.Increment(n)
	P.sha.Write(p[0:n])
	return n, err
}

func (P *ProgressTrackReader) SumSha() []byte {
	return P.sha.Sum(nil)
}

func (J *EncodeWorker) UploadJob(task *model.WorkTaskEncode, track *TaskTracks) error {
	J.updateTaskStatus(task, model.UploadNotification, model.ProgressingNotificationStatus, "")
	err := retry.Do(func() error {
		track.UpdateValue(0)
		encodedFile, err := os.Open(task.TargetFilePath)
		if err != nil {
			return err
		}
		defer encodedFile.Close()
		fi, _ := encodedFile.Stat()
		fileSize := fi.Size()
		track.SetTotal(fileSize)
		sha := sha256.New()
		if _, err := io.Copy(sha, encodedFile); err != nil {
			return err
		}
		checksum := hex.EncodeToString(sha.Sum(nil))
		encodedFile.Seek(0, io.SeekStart)

		reader := NewProgressTrackStream(track, encodedFile)

		client := &http.Client{}
		//go printProgress(J.ctx, reader, fileSize, wg, "Uploading")
		req, err := http.NewRequestWithContext(J.ctx, "POST", task.TaskEncode.UploadURL, reader)
		if err != nil {
			return err
		}
		req.ContentLength = fileSize
		req.Body = reader
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(reader), nil
		}

		req.Header.Add("checksum", checksum)
		req.Header.Add("Content-Type", "application/octet-stream")
		req.Header.Add("Content-Length", strconv.FormatInt(fileSize, 10))
		resp, err := client.Do(req)

		if err != nil {
			return err
		}
		//wg.Wait()
		if resp.StatusCode != 201 {
			return fmt.Errorf("invalid status code %d", resp.StatusCode)
		}
		track.UpdateValue(fileSize)
		return nil
	}, retry.Delay(time.Second*5),
		retry.RetryIf(func(err error) bool {
			return !errors.Is(err, context.Canceled)
		}),
		retry.DelayType(retry.FixedDelay),
		retry.Attempts(17280),
		retry.LastErrorOnly(true),
		retry.OnRetry(func(n uint, err error) {
			J.terminal.Error("error on uploading job %s", err.Error())
		}))

	if err != nil {
		J.updateTaskStatus(task, model.UploadNotification, model.FailedNotificationStatus, "")
		return err
	}

	J.updateTaskStatus(task, model.UploadNotification, model.CompletedNotificationStatus, "")
	return nil
}

func (J *EncodeWorker) errorJob(taskEncode *model.WorkTaskEncode, err error) {
	if errors.Is(err, context.Canceled) {
		J.updateTaskStatus(taskEncode, model.JobNotification, model.CanceledNotificationStatus, "")
	} else {
		J.updateTaskStatus(taskEncode, model.JobNotification, model.FailedNotificationStatus, err.Error())
	}

	taskEncode.Clean()
}

func (J *EncodeWorker) Execute(workData []byte) error {
	taskEncode := &model.TaskEncode{}
	err := json.Unmarshal(workData, taskEncode)
	if err != nil {
		return err
	}
	workDir := filepath.Join(J.tempPath, taskEncode.Id.String())
	workTaskEncode := &model.WorkTaskEncode{
		TaskEncode: taskEncode,
		WorkDir:    workDir,
	}
	os.MkdirAll(workDir, os.ModePerm)

	J.updateTaskStatus(workTaskEncode, model.JobNotification, model.ProgressingNotificationStatus, "")
	J.AddDownloadJob(workTaskEncode)
	return nil
}

func (J *EncodeWorker) Cancel() {
	J.cancelContext()
}
func (J *EncodeWorker) StopQueues() {
	defer close(J.downloadChan)
	defer close(J.uploadChan)
	defer close(J.encodeChan)
	J.stopQueues()
	J.wg.Wait()
}
func (J *EncodeWorker) GetID() string {
	return J.name
}
func (J *EncodeWorker) updateTaskStatus(encode *model.WorkTaskEncode, notificationType model.NotificationType, status model.NotificationStatus, message string) {
	encode.TaskEncode.EventID++
	event := model.TaskEvent{
		Id:               encode.TaskEncode.Id,
		EventID:          encode.TaskEncode.EventID,
		EventType:        model.NotificationEvent,
		WorkerName:       J.workerConfig.Name,
		EventTime:        time.Now(),
		NotificationType: notificationType,
		Status:           status,
		Message:          message,
	}
	J.Manager.EventNotification(event)
	J.terminal.Log("[%s] %s has been %s: %s", event.Id.String(), event.NotificationType, event.Status, event.Message)

	J.saveTaskStatusDisk(&model.TaskStatus{
		LastState: &event,
		Task:      encode,
	})

}

func (J *EncodeWorker) saveTaskStatusDisk(taskEncode *model.TaskStatus) {
	J.mu.Lock()
	defer J.mu.Unlock()
	b, err := json.MarshalIndent(taskEncode, "", "\t")
	if err != nil {
		panic(err)
	}
	eventFile, err := os.OpenFile(filepath.Join(taskEncode.Task.WorkDir, fmt.Sprintf("%s.json", taskEncode.Task.TaskEncode.Id)), os.O_TRUNC|os.O_CREATE|os.O_RDWR, os.ModePerm)
	if err != nil {
		return
	}
	defer eventFile.Close()
	eventFile.Write(b)
	eventFile.Sync()
}
func (J *EncodeWorker) readTaskStatusFromDiskByPath(filepath string) *model.TaskStatus {
	eventFile, err := os.Open(filepath)
	if err != nil {
		panic(err)
	}
	defer eventFile.Close()
	b, err := io.ReadAll(eventFile)
	if err != nil {
		panic(err)
	}
	taskStatus := &model.TaskStatus{}
	err = json.Unmarshal(b, taskStatus)
	if err != nil {
		panic(err)
	}
	return taskStatus
}

func (J *EncodeWorker) PGSMkvExtractDetectAndConvert(taskEncode *model.WorkTaskEncode, track *TaskTracks, container *ContainerData) error {
	var PGSTOSrt []*Subtitle
	for _, subt := range container.Subtitle {
		if subt.isImageTypeSubtitle() {
			PGSTOSrt = append(PGSTOSrt, subt)
		}
	}
	if len(PGSTOSrt) > 0 {
		J.updateTaskStatus(taskEncode, model.MKVExtractNotification, model.ProgressingNotificationStatus, "")
		track.Message(string(model.MKVExtractNotification))
		track.SetTotal(0)
		err := J.MKVExtract(PGSTOSrt, taskEncode)
		if err != nil {
			J.updateTaskStatus(taskEncode, model.MKVExtractNotification, model.FailedNotificationStatus, err.Error())
			return err
		}
		J.updateTaskStatus(taskEncode, model.MKVExtractNotification, model.CompletedNotificationStatus, "")

		log.Debug("is going to start PGS task?")
		J.updateTaskStatus(taskEncode, model.PGSNotification, model.ProgressingNotificationStatus, "")
		track.Message(string(model.PGSNotification))
		log.Debugf("converting PGS to SRT: %+v", PGSTOSrt)
		err = J.convertPGSToSrt(taskEncode, container, PGSTOSrt)
		if err != nil {
			J.updateTaskStatus(taskEncode, model.PGSNotification, model.FailedNotificationStatus, err.Error())
			return err
		} else {
			J.updateTaskStatus(taskEncode, model.PGSNotification, model.CompletedNotificationStatus, "")
		}
	}
	return nil
}

func (J *EncodeWorker) convertPGSToSrt(taskEncode *model.WorkTaskEncode, container *ContainerData, subtitles []*Subtitle) error {
	log.Debug("convert PGS to SRT")
	out := make(chan *model.TaskPGSResponse)
	var pendingPGSResponses []<-chan *model.TaskPGSResponse
	for _, subtitle := range subtitles {
		log.Debugf("starting to process subtitle %+v", subtitle)
		subFile, err := os.Open(filepath.Join(taskEncode.WorkDir, fmt.Sprintf("%d.sup", subtitle.Id)))
		if err != nil {
			return err
		}
		outputBytes, err := io.ReadAll(subFile)
		if err != nil {
			return err
		}
		subFile.Close()
		log.Debugf("subtitle %d is pgs, requesting conversion", subtitle.Id)

		PGSResponse := J.RequestPGSJob(model.TaskPGS{
			Id:          taskEncode.TaskEncode.Id,
			PGSID:       int(subtitle.Id),
			PGSdata:     outputBytes,
			PGSLanguage: subtitle.Language,
		})
		pendingPGSResponses = append(pendingPGSResponses, PGSResponse)
	}
	go func() {
		for _, c := range pendingPGSResponses {
			for v := range c {
				out <- v
			}
		}
		close(out)
	}()

	log.Debug("start the PGs counter")
	for {
		select {
		case <-J.ctx.Done():
			return J.ctx.Err()
		case <-time.After(time.Minute * 90):
			return errors.New("timeout waiting for PGS job done")
		case response, ok := <-out:
			if !ok {
				return nil
			}
			log.Debugf("response: %+v", response)
			if response.Err != "" {
				return fmt.Errorf("error on process PGS %d: %s", response.PGSID, response.Err)
			}
			subtFilePath := filepath.Join(taskEncode.WorkDir, fmt.Sprintf("%d.srt", response.PGSID))
			err := os.WriteFile(subtFilePath, response.Srt, os.ModePerm)
			if err != nil {
				return err
			}
		}
	}
}

func (J *EncodeWorker) MKVExtract(subtitles []*Subtitle, taskEncode *model.WorkTaskEncode) error {
	mkvExtractCommand := command.NewCommand(helper.GetMKVExtractPath(), "tracks", taskEncode.SourceFilePath).
		SetWorkDir(taskEncode.WorkDir)
	if runtime.GOOS == "linux" {
		mkvExtractCommand.AddEnv(fmt.Sprintf("LD_LIBRARY_PATH=%s", filepath.Dir(helper.GetMKVExtractPath())))
	}
	for _, subtitle := range subtitles {
		mkvExtractCommand.AddParam(fmt.Sprintf("%d:%d.sup", subtitle.Id, subtitle.Id))
	}

	_, err := mkvExtractCommand.RunWithContext(J.ctx, command.NewAllowedCodesOption(0, 1))
	if err != nil {
		J.terminal.Cmd("MKVExtract command:%s", mkvExtractCommand.GetFullCommand())
		return fmt.Errorf("MKVExtract unexpected error:%v", err.Error())
	}

	return nil
}
func (J *EncodeWorker) PrefetchJobs() uint32 {
	return atomic.LoadUint32(&J.prefetchJobs)
}

func (J *EncodeWorker) AddDownloadJob(job *model.WorkTaskEncode) {
	log.Debug("add another download job")
	atomic.AddUint32(&J.prefetchJobs, 1)
	J.downloadChan <- job
}

func (J *EncodeWorker) downloadQueue() {
	J.wg.Add(1)
	for {
		select {
		case <-J.ctx.Done():
		case <-J.ctxStopQueues.Done():
			J.terminal.Warn("stopping download queue")
			J.wg.Done()
			return
		case job, ok := <-J.downloadChan:
			if !ok {
				continue
			}

			taskTrack := J.terminal.AddTask(job.TaskEncode.Id.String(), DownloadJobStepType)

			J.updateTaskStatus(job, model.DownloadNotification, model.ProgressingNotificationStatus, "")
			err := J.downloadFile(job, taskTrack)
			if err != nil {
				J.updateTaskStatus(job, model.DownloadNotification, model.FailedNotificationStatus, err.Error())
				taskTrack.Error()
				J.errorJob(job, err)
				atomic.AddUint32(&J.prefetchJobs, ^uint32(0))
				continue
			}
			J.updateTaskStatus(job, model.DownloadNotification, model.CompletedNotificationStatus, "")
			taskTrack.Done()
			J.encodeChan <- job
		}
	}

}

func (J *EncodeWorker) uploadQueue() {
	J.wg.Add(1)
	for {
		select {
		case <-J.ctx.Done():
		case <-J.ctxStopQueues.Done():
			J.terminal.Warn("stopping upload queue")
			J.wg.Done()
			return
		case job, ok := <-J.uploadChan:
			if !ok {
				continue
			}
			taskTrack := J.terminal.AddTask(job.TaskEncode.Id.String(), UploadJobStepType)
			err := J.UploadJob(job, taskTrack)
			if err != nil {
				taskTrack.Error()
				J.errorJob(job, err)
				continue
			}

			J.updateTaskStatus(job, model.JobNotification, model.CompletedNotificationStatus, "")
			taskTrack.Done()
			job.Clean()
		}
	}

}

func (J *EncodeWorker) encodeQueue() {
	J.wg.Add(1)
	for {
		select {
		case <-J.ctx.Done():
		case <-J.ctxStopQueues.Done():
			J.terminal.Warn("stopping encode queue")
			J.wg.Done()
			return
		case job, ok := <-J.encodeChan:
			if !ok {
				continue
			}
			atomic.AddUint32(&J.prefetchJobs, ^uint32(0))
			taskTrack := J.terminal.AddTask(job.TaskEncode.Id.String(), EncodeJobStepType)
			err := J.encodeVideo(job, taskTrack)
			if err != nil {
				taskTrack.Error()
				J.errorJob(job, err)
				continue
			}

			taskTrack.Done()
			J.uploadChan <- job
		}
	}

}

func (J *EncodeWorker) encodeVideo(job *model.WorkTaskEncode, track *TaskTracks) error {
	J.updateTaskStatus(job, model.FFProbeNotification, model.ProgressingNotificationStatus, "")
	track.Message(string(model.FFProbeNotification))
	sourceVideoParams, sourceVideoSize, err := J.getVideoParameters(job.SourceFilePath)
	if err != nil {
		J.updateTaskStatus(job, model.FFProbeNotification, model.FailedNotificationStatus, err.Error())
		return err
	}
	J.updateTaskStatus(job, model.FFProbeNotification, model.CompletedNotificationStatus, "")

	videoContainer, err := J.clearData(sourceVideoParams)
	if err != nil {
		J.terminal.Warn("error in clear data. Id: %s", J.GetID())
		return err
	}
	if err = J.PGSMkvExtractDetectAndConvert(job, track, videoContainer); err != nil {
		return err
	}
	J.updateTaskStatus(job, model.FFMPEGSNotification, model.ProgressingNotificationStatus, "")
	track.ResetMessage()
	track.SetTotal(int64(videoContainer.Video.Duration.Seconds()) * int64(videoContainer.Video.FrameRate))
	FFMPEGProgressChan := make(chan FFMPEGProgress)

	go func() {
		lastProgressEvent := float64(0)
		lastDuration := 0
	loop:
		for {
			select {
			case <-J.ctx.Done():
				return
			case FFMPEGProgress, open := <-FFMPEGProgressChan:
				if !open {
					break loop
				}
				encodeFramesIncrement := (FFMPEGProgress.duration - lastDuration) * videoContainer.Video.FrameRate
				lastDuration = FFMPEGProgress.duration

				track.Increment(encodeFramesIncrement)

				if FFMPEGProgress.percent-lastProgressEvent > 10 {
					J.updateTaskStatus(job, model.FFMPEGSNotification, model.ProgressingNotificationStatus, fmt.Sprintf("{\"progress\":\"%.2f\"}", track.PercentDone()))
					lastProgressEvent = FFMPEGProgress.percent
				}
			}
		}
	}()
	err = J.FFMPEG(job, videoContainer, FFMPEGProgressChan)
	if err != nil {
		//<-time.After(time.Minute*30)
		J.updateTaskStatus(job, model.FFMPEGSNotification, model.FailedNotificationStatus, err.Error())
		return err
	}
	<-time.After(time.Second * 1)

	encodedVideoParams, encodedVideoSize, err := J.getVideoParameters(job.TargetFilePath)
	if err != nil {
		J.updateTaskStatus(job, model.FFMPEGSNotification, model.FailedNotificationStatus, err.Error())
		return err
	}
	diffDuration := encodedVideoParams.Format.DurationSeconds - sourceVideoParams.Format.DurationSeconds
	if diffDuration > 60 || diffDuration < -60 {
		err = fmt.Errorf("source file duration %f is diferent than encoded %f", sourceVideoParams.Format.DurationSeconds, encodedVideoParams.Format.DurationSeconds)
		J.updateTaskStatus(job, model.FFMPEGSNotification, model.FailedNotificationStatus, err.Error())
		return err
	}
	if encodedVideoSize > sourceVideoSize {
		err = fmt.Errorf("source file size %d bytes is less than encoded %d bytes", sourceVideoSize, encodedVideoSize)
		J.updateTaskStatus(job, model.FFMPEGSNotification, model.FailedNotificationStatus, err.Error())
		return err
	}
	J.updateTaskStatus(job, model.FFMPEGSNotification, model.CompletedNotificationStatus, "")
	return nil
}

/*func (J *EncodeWorker) isQueueFull() bool {
	return len(J.encodeChan) >= MAX_PREFETCHED_JOBS || len(J.downloadChan) > 0
}*/

type FFMPEGGenerator struct {
	inputPaths     []string
	VideoFilter    string
	AudioFilter    []string
	SubtitleFilter []string
	Metadata       string
}

func (F *FFMPEGGenerator) setAudioFilters(container *ContainerData) {

	for index, audioStream := range container.Audios {
		//TODO que pasa quan el channelLayout esta empty??
		title := fmt.Sprintf("%s (%s)", audioStream.Language, audioStream.ChannelLayour)
		metadata := fmt.Sprintf(" -metadata:s:a:%d \"title=%s\"", index, title)
		codecQuality := fmt.Sprintf("-c:a:%d %s -vbr %d", index, "libfdk_aac", 5)
		F.AudioFilter = append(F.AudioFilter, fmt.Sprintf(" -map 0:%d %s %s", audioStream.Id, metadata, codecQuality))
	}
}
func (F *FFMPEGGenerator) setVideoFilters(container *ContainerData) {
	// TODO: Make ffmpeg parameters configurable
	videoFilterParameters := "\"scale='min(1920,iw)':-1:force_original_aspect_ratio=decrease\""
	videoEncoderQuality := "-pix_fmt yuv420p10le -c:v libx265 -crf 28 -x265-params profile=main10"
	//TODO HDR??
	videoHDR := ""
	F.VideoFilter = fmt.Sprintf("-map 0:%d -map_chapters -1 -flags +global_header -filter:v %s %s %s", container.Video.Id, videoFilterParameters, videoHDR, videoEncoderQuality)

}
func (F *FFMPEGGenerator) setSubtFilters(container *ContainerData) {
	subtInputIndex := 1
	for index, subtitle := range container.Subtitle {
		if subtitle.isImageTypeSubtitle() {

			subtitleMap := fmt.Sprintf("-map %d -c:s:%d srt", subtInputIndex, index)
			subtitleForced := ""
			subtitleComment := ""
			if subtitle.Forced {
				subtitleForced = fmt.Sprintf(" -disposition:s:s:%d forced  -disposition:s:s:%d default", index, index)
			}
			if subtitle.Comment {
				subtitleComment = fmt.Sprintf(" -disposition:s:s:%d comment", index)
			}

			F.SubtitleFilter = append(F.SubtitleFilter, fmt.Sprintf("%s %s %s -metadata:s:s:%d language=%s -metadata:s:s:%d \"title=%s\" -max_interleave_delta 0", subtitleMap, subtitleForced, subtitleComment, index, subtitle.Language, index, subtitle.Title))
			subtInputIndex++
		} else {
			F.SubtitleFilter = append(F.SubtitleFilter, fmt.Sprintf("-map 0:%d -c:s:%d copy", subtitle.Id, index))
		}

	}
}
func (F *FFMPEGGenerator) setMetadata(container *ContainerData) {
	F.Metadata = fmt.Sprintf("-metadata encodeParameters='%s'", container.ToJson())
}
func (F *FFMPEGGenerator) buildArguments(threads uint8, outputFilePath string) string {
	coreParameters := fmt.Sprintf("-hide_banner  -threads %d", threads)
	inputsParameters := ""
	for _, input := range F.inputPaths {
		inputsParameters = fmt.Sprintf("%s -i \"%s\"", inputsParameters, input)
	}
	//-ss 900 -t 10
	audioParameters := ""
	for _, audio := range F.AudioFilter {
		audioParameters = fmt.Sprintf("%s %s", audioParameters, audio)
	}
	subtParameters := ""
	for _, subt := range F.SubtitleFilter {
		subtParameters = fmt.Sprintf("%s %s", subtParameters, subt)
	}

	return fmt.Sprintf("%s %s -max_muxing_queue_size 9999 %s %s %s %s %s -y", coreParameters, inputsParameters, F.VideoFilter, audioParameters, subtParameters, F.Metadata, outputFilePath)
}

func (F *FFMPEGGenerator) setInputFilters(container *ContainerData, sourceFilePath string, tempPath string) {
	F.inputPaths = append(F.inputPaths, sourceFilePath)
	inputIndex := 0
	if container.HaveImageTypeSubtitle() {
		for _, subt := range container.Subtitle {
			if subt.isImageTypeSubtitle() {
				inputIndex++
				F.inputPaths = append(F.inputPaths, filepath.Join(tempPath, fmt.Sprintf("%d.srt", subt.Id)))
			}
		}
	}
}

type Video struct {
	Id        uint8
	Duration  time.Duration
	FrameRate int
}
type Audio struct {
	Id             uint8
	Language       string
	Channels       string
	ChannelsNumber uint8
	ChannelLayour  string
	Default        bool
	Bitrate        uint
	Title          string
}
type Subtitle struct {
	Id       uint8
	Language string
	Forced   bool
	Comment  bool
	Format   string
	Title    string
}
type ContainerData struct {
	Video    *Video
	Audios   []*Audio
	Subtitle []*Subtitle
}

func (C *ContainerData) HaveImageTypeSubtitle() bool {
	for _, sub := range C.Subtitle {
		if sub.isImageTypeSubtitle() {
			return true
		}
	}
	return false
}
func (C *ContainerData) ToJson() string {
	b, err := json.Marshal(C)
	if err != nil {
		panic(err)
	}
	return string(b)
}
func (C *Subtitle) isImageTypeSubtitle() bool {
	return strings.Index(strings.ToLower(C.Format), "pgs") != -1
}
