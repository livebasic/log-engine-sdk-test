package watch

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/fsnotify/fsnotify"
	"log-engine-sdk/pkg/k3"
	"log-engine-sdk/pkg/k3/config"
	"log-engine-sdk/pkg/k3/protocol"
	"log-engine-sdk/pkg/k3/sender"
	"os"
	"sync"
	"time"
)

type FileState struct {
	Path          string
	Offset        int64
	StartReadTime int64
	LastReadTime  int64
	IndexName     string
}

func (f *FileState) String() string {
	return fmt.Sprintf("Path: %s, Offset: %d, StartReadTime: %d, LastReadTime: %d, IndexName: %s", f.Path, f.Offset, f.StartReadTime, f.LastReadTime, f.IndexName)
}

// 处理不同类型的协程回收工作
var (
	ClockWG   *sync.WaitGroup // 定时器协程的等待退出
	WatcherWG *sync.WaitGroup // Watch协程的等待退出
)

// 处理全局资源的并发问题, 确保GlobalFileStates数据的变更是原子的
var (
	GlobalFileStatesLock *sync.Mutex           // 控制GlobalFileStates的锁
	FileStateFilePath    string                // GlobalFileStates 硬盘存储状态文件路径
	GlobalFileStates     map[string]*FileState // 对应监控的所有文件的状态，映射 core.json文件
)

// 处理不同类型的协程主动退出的问题
var (
	WatcherContext       context.Context    // 控制watcher相关所有协程退出
	WatcherContextCancel context.CancelFunc // 用于主动取消watcher相关的所有协程（含Clock协程）
)

var (
	GlobalDataAnalytics k3.DataAnalytics // 日志接收器
	DefaultSyncInterval = 60             // 单位秒, 默认为60s, 默认定时60秒将GlobalFileStates的状态同步到硬盘
	DefaultMaxReadCount = 200            // 默认每次读取日志文件的最大次数
)

var (
	processingSem chan struct{} // 可开启的最大协程数量
	processingWg  *sync.WaitGroup
	processingMap *sync.Map
)

// TODO 定时处理文件已经读完，或者长时间为读取的情况, 考虑如果文件长时间为读取，读取完以后，是否要删除GlobalFileState中文件的问题, 还是说删除工作一句硬盘文件真实被删除来处理
var (
	// obsolete_interval : 1 # 单位小时, 默认1  定时1小时检查一下GlobalFileState中，是否文件是不是有已经读取完的
	// obsolete_date : 1 	 # 单位天，  默认1， 表示如果文件一天都没有读写，表示已经没有写入了
	// obsolete_max_read_count : 1000  # 对于长时间没有读写的文件， 一次最大读取次数

	DefaultObsoleteInterval     = 1
	DefaultObsoleteDate         = 1    // 单位天， 默认1， 表示文件如果1天没有写入, 就查看下是不是读取完了，没读完就读完整个文件.
	DefaultObsoleteMaxReadCount = 5000 // 对于长时间没有读写的文件， 一次最大读取次数
)

func InitVars() {
	ClockWG = &sync.WaitGroup{}                                                          // 定时器协程锁
	WatcherWG = &sync.WaitGroup{}                                                        // Watcher协程锁
	GlobalFileStatesLock = &sync.Mutex{}                                                 // 全局FileStates锁
	FileStateFilePath = k3.GetRootPath() + "/" + config.GlobalConfig.Watch.StateFilePath // Watcher读写硬盘的状态文件记录地址
	GlobalFileStates = make(map[string]*FileState)                                       // 初始化全局FileStates

	WatcherContext, WatcherContextCancel = context.WithCancel(context.Background()) // Watcher取消上下文

	processingMap = &sync.Map{}
	processingWg = &sync.WaitGroup{}
	processingSem = make(chan struct{}, 100) // 控制最大协程数量为100
}

func InitConsumerBatchLog() error {
	var (
		elk      *sender.ElasticSearchClient
		err      error
		consumer protocol.K3Consumer
	)
	if elk, err = sender.NewElasticsearch(config.GlobalConfig.ELK.Address,
		config.GlobalConfig.ELK.Username,
		config.GlobalConfig.ELK.Password); err != nil {
		return err
	}

	if consumer, err = k3.NewBatchConsumerWithConfig(k3.K3BatchConsumerConfig{
		Sender:        elk,
		BatchSize:     config.GlobalConfig.Consumer.ConsumerBatchSize,
		AutoFlush:     config.GlobalConfig.Consumer.ConsumerBatchAutoFlush,
		Interval:      config.GlobalConfig.Consumer.ConsumerBatchInterval,
		CacheCapacity: config.GlobalConfig.Consumer.ConsumerBatchCapacity,
	}); err != nil {
		return err
	}
	GlobalDataAnalytics = k3.NewDataAnalytics(consumer)

	return nil
}

// LoadDiskFileToGlobalFileStates 从文件加载GlobalFileStates内存中
func LoadDiskFileToGlobalFileStates(filePath string) error {
	var (
		fd      *os.File
		decoder *json.Decoder
		err     error
	)

	GlobalFileStatesLock.Lock()
	defer GlobalFileStatesLock.Unlock()

	// 打开文件
	if fd, err = os.OpenFile(filePath, os.O_RDWR, os.ModePerm); err != nil {
		return errors.New("[LoadDiskFileToGlobalFileStates] open state file failed: " + err.Error())
	}
	defer fd.Close()

	// 将文件映射到FileState
	decoder = json.NewDecoder(fd)

	if err = decoder.Decode(&GlobalFileStates); err != nil {
		return errors.New("[LoadDiskFileToGlobalFileStates] json decode failed: " + err.Error())
	}

	return nil
}

// SaveGlobalFileStatesToDiskFile 保存GlobalFileState的数据到硬盘目录filePath
func SaveGlobalFileStatesToDiskFile(filePath string) error {
	var (
		fd      *os.File
		encoder *json.Encoder
		err     error
	)

	GlobalFileStatesLock.Lock()
	defer GlobalFileStatesLock.Unlock()

	// 打开文件, 并清空
	if fd, err = os.OpenFile(filePath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, os.ModePerm); err != nil {
		return errors.New("[SaveFileStateToDiskFile] open state file failed: " + err.Error())
	}
	defer fd.Close()

	encoder = json.NewEncoder(fd)

	if err = encoder.Encode(&GlobalFileStates); err != nil {
		return errors.New("[SaveFileStateToDiskFile] json encode failed: " + err.Error())
	}

	k3.K3LogDebug("[SaveFileStateToDiskFile] save file state to disk file success .")
	return nil
}

// ScanLogFileToGlobalFileStatesAndSaveToDiskFile  保证硬盘文件和FileState一致，并同步到硬盘状态文件, 项目启动的时候使用此函数，未加锁
func ScanLogFileToGlobalFileStatesAndSaveToDiskFile(directory map[string][]string, filePath string) error {
	var (
		totalFiles           = make(map[string][]string)
		err                  error
		files                []string
		globalFileStatesKeys []string
		tempDiskFiles        []string
	)

	globalFileStatesInterface := make(map[string]interface{})
	for k, fileState := range GlobalFileStates {
		globalFileStatesInterface[k] = fileState
	}
	// 获取GlobalFileStates的key
	globalFileStatesKeys = k3.GetMapKeys(globalFileStatesInterface)

	for indexName, dirs := range directory {

		for _, dir := range dirs {
			if files, err = k3.FetchDirectory(dir, -1); err != nil {
				continue
			}
			totalFiles[indexName] = append(totalFiles[indexName], files...)
		}
	}

	// 检查硬盘上的日志文件是否存在GlobalFileStates中，如果不存在就ADD
	for indexName, diskFiles := range totalFiles {
		tempDiskFiles = append(tempDiskFiles, diskFiles...)
		for _, diskFile := range diskFiles {
			if k3.InSlice(diskFile, globalFileStatesKeys) == false {
				GlobalFileStates[diskFile] = &FileState{
					Path:          diskFile,
					Offset:        0,
					StartReadTime: 0,
					LastReadTime:  0,
					IndexName:     indexName,
				}
			}
		}
	}

	// 检查GlobalFileStates中是否真实存在于硬盘上，如果不存在就DELETE
	for _, fileStateKey := range globalFileStatesKeys {
		if k3.InSlice(fileStateKey, tempDiskFiles) == false {
			delete(GlobalFileStates, fileStateKey)
		}
	}

	if err = SaveGlobalFileStatesToDiskFile(filePath); err != nil {
		return errors.New("[ScanDiskLogAddFileState] save file state to disk failed: " + err.Error())
	}

	return nil
}

// InitWatcher 每个indexName 开一个协程
// directory: map[indexName][]dir 每个索引对应的需要监控的所有目录
// fileStatePath: GlobalFileStates状态文件路径
func InitWatcher(directory map[string][]string, fileStatePath string) error {

	// TODO 这里要考虑2个问题，
	// TODO 1. watcher协程在初始化的时候, 并不是所有的协程都创建成功，这样就需要终止后面所有的协程创建，并让已经创建的协程回收，且终止主程序
	// TODO 2. 如果所有的协程创建成功， 一旦某个协程出现异常，需要让所有的协程退出，并回收，且终止主程序

	var (
		// 定义检查所有协程是否创建成功的chan
		isSuccess = make(chan error, len(directory))
		err       error
	)

	// 每个index name 开一个协程来处理监听事件
	for indexName, dirs := range directory {
		WatcherWG.Add(1)
		go forkWatcher(indexName, dirs, fileStatePath, isSuccess)
	}

	// 用于解决，主程序启动后，一旦有一个协程异常退出，用于回收协程，并让其他协程也退出
	go func() {
		WatcherWG.Wait() // 阻塞函数
		k3.K3LogInfo("[InitWatcher] All watcher goroutine exit.")
		WatcherContextCancel() // 考虑到所有的Watcher的协程都退出了， 保险起见再次发一个退出信号
	}()

	// 判断协程开启的协程是否都创建成功， 如果有一个不成功就直接 退出主程序
	for i := 0; i < len(directory); i++ {
		if err = <-isSuccess; err != nil {
			k3.K3LogError("[InitWatcher] watcher goroutine exit: %s", err.Error())
			WatcherContextCancel()
			break
		}
	}
	close(isSuccess)

	return err
}

// forkWatcher 开单一协程来处理监听，每个indexName开一个协程
func forkWatcher(indexName string, dirs []string, fileStatePath string, isSuccess chan error) {
	var (
		watcher *fsnotify.Watcher
		err     error
	)

	defer WatcherWG.Done()
	defer WatcherContextCancel()

	// 每个indexName 创建一个Watcher
	if watcher, err = fsnotify.NewWatcher(); err != nil {
		// 处理错误，让所有的Watcher协程退出
		k3.K3LogError("[forkWatcher] new watcher failed: %s", err.Error())
		WatcherContextCancel()
		isSuccess <- err
		return
	}
	defer watcher.Close()

	// 将所有的目录都加入监听
	for _, dir := range dirs {
		if err = watcher.Add(dir); err != nil {
			// 处理错误， 让所有的Watcher协程退出
			k3.K3LogError("[forkWatcher] add dir to watcher failed: %s", err.Error())
			WatcherContextCancel()
			isSuccess <- err
			return
		}
	}

	// 证明协程已经创建成功，将成功信号返回
	isSuccess <- nil

EXIT:
	for { //  阻塞函数块
		select {

		case event, ok := <-watcher.Events:
			if !ok {
				k3.K3LogWarn("[forkWatcher] index_name[%s] watcher event channel closed.", indexName)
				WatcherContextCancel()
				break EXIT
			}
			// 处理Event
			handlerEvent(indexName, event, fileStatePath, watcher)

		case err, ok := <-watcher.Errors:
			if !ok {
				k3.K3LogWarn("[forkWatcher] index_name[%s] watcher error channel closed.", indexName)
				WatcherContextCancel()
				break EXIT
			}

			k3.K3LogError("[forkWatcher] index_name[%s] watcher error: %s", indexName, err)
			WatcherContextCancel()
			break EXIT

		case <-WatcherContext.Done():
			k3.K3LogWarn("[forkWatcher] index_name[%s] watcher exit with by globalWatchContext. ", indexName)
			break EXIT
		}
	}

	return
}

// TODO 处理EVENT事件
func handlerEvent(indexName string, event fsnotify.Event, fileStatePath string, watcher *fsnotify.Watcher) {
	// 删除 -> 删除GlobalFileState的内容

	// 新增 -> 目录就add监听

	// 修改 -> 读取文件，更新GlobalFileState, 并把数据发送给elk
	if event.Op&fsnotify.Write == fsnotify.Write {
		// fmt.Println("收到变更", indexName, event.Name)
		writeEvent(indexName, event)
	} else if event.Op&fsnotify.Create == fsnotify.Create {
		// fmt.Println("收到新增", indexName, event.Name)
		createEvent(indexName, event, watcher)
	} else if event.Op&fsnotify.Remove == fsnotify.Remove || event.Op&fsnotify.Rename == fsnotify.Rename {
		// fmt.Println("收到删除或修改文件名称", indexName, event.Name)
		removeEvent(event, watcher)
	}
}

// ReadFileByOffset 读取文件
func ReadFileByOffset(indexName string, event fsnotify.Event) {
	defer processingWg.Done()

	// 1. 判断当前协程数量是否负载, 如果负载， processingSem会阻塞，等待其他协程处理完
	processingSem <- struct{}{}
	defer func() {
		<-processingSem
	}()

	// 2. 判断当前文件是不是已经在协程中
	if _, loading := processingMap.LoadOrStore(event.Name, true); loading {
		k3.K3LogWarn("[ReadFileOffset] %s is already being processed, skipping .", event.Name)
		return
	}

	// 3. 开始处理读取发送问题
	var (
		maxReadCount     = config.GlobalConfig.Watch.MaxReadCount
		currentReadCount int
		currentOffset    int64
		reader           *bufio.Reader
		content          string
	)

	if maxReadCount < 0 || maxReadCount > DefaultMaxReadCount {
		maxReadCount = DefaultMaxReadCount
	}
	// 3.1. 打开文件

	// 3.2. 根据GlobalFileState的offset开始循环读取文件，读取次数为maxReadCount

	// 3.3. 将读取的数据，发送给ELK
	// 3.4. 协程结束，将当前文件的协程移除
	processingMap.Delete(event.Name)
}

// 日志写入的监听
func writeEvent(indexName string, event fsnotify.Event) {
	// TODO 文件写入被监听到，如果文件在GlobalFileState中，就读取文件，如果不存在，就优先将文件写入到GlobalFileStates中，并强制同步到硬盘

	if _, exists := GlobalFileStates[event.Name]; !exists {
		GlobalFileStatesLock.Lock()
		GlobalFileStates[event.Name] = &FileState{
			Path:          event.Name,
			Offset:        0,
			StartReadTime: time.Now().Unix(),
			LastReadTime:  time.Now().Unix(),
			IndexName:     indexName,
		}
		if err := SaveGlobalFileStatesToDiskFile(FileStateFilePath); err != nil {
			k3.K3LogError("[writeEvent] index_name[%s] event[%s] path[%s] save to disk file failed: %s", indexName, event.Op, event.Name, err.Error())
		}
		GlobalFileStatesLock.Unlock()
	}

	processingWg.Add(1)
	// 监测到某个文件有写入，循环读取
	go ReadFileByOffset(indexName, event)
}

// 文件或目录创建
func createEvent(indexName string, event fsnotify.Event, watcher *fsnotify.Watcher) {
	var (
		err error
		ok  bool
	)
	// 如果是目录就添加监听， 如果是文件就将文件写入FileStates中，并强制更新一次硬盘
	if ok, err = k3.IsDirectory(event.Name); err != nil {
		// 如果这里报错，有可能会导致文件或者目录不会被监听，记录下日志
		k3.K3LogError("[createEvent] index_name[%s] event[%s] path[%s] failed : %s", indexName, event.Op, event.Name, err.Error())
		return
	} else {
		// fmt.Println("WRITE", "==>", event.Name)
		if ok {
			// 将目录加入到监听
			if err = watcher.Add(event.Name); err != nil {
				k3.K3LogError("[createEvent] index_name[%s] event[%s] path[%s] add watcher failed: %s", indexName, event.Op, event.Name, err.Error())
				return
			}
		} else {
			// 将文件写入到GlobalFileStates中, 无需同步给硬盘，交给定时器处理同步工作
			GlobalFileStatesLock.Lock()
			GlobalFileStates[event.Name] = &FileState{
				Path:          event.Name,
				Offset:        0,
				StartReadTime: 0,
				LastReadTime:  0,
				IndexName:     indexName,
			}
			GlobalFileStatesLock.Unlock()
		}
	}
}

// 文件或目录删除
func removeEvent(event fsnotify.Event, watcher *fsnotify.Watcher) {
	// 如果是目录，删除watcher的监听， 如果是文件，删除文件FileStates中的记录
	// 注意， 当文件被删除或者改名，原来的文件其实已经被删除了, 那再去判断文件是什么类型已经没有意义了，所以需要直接处理
	GlobalFileStatesLock.Lock()
	delete(GlobalFileStates, event.Name)
	GlobalFileStatesLock.Unlock()
	// 这里没有判断是不是目录了， 无所谓，直接删了就行了
	_ = watcher.Remove(event.Name)
	// fmt.Println(event.Name, "------>", watcher.WatchList())
}

// ClockSyncGlobalFileStatesToDiskFile 定时将GlobalFileStates数据同步到硬盘
func ClockSyncGlobalFileStatesToDiskFile(filePath string) {
	// 创建定时器
	var (
		syncInterval = config.GlobalConfig.Watch.SyncInterval
		t            *time.Ticker
		err          error
	)

	if syncInterval < 0 || syncInterval > DefaultSyncInterval {
		syncInterval = DefaultSyncInterval
	}

	t = time.NewTicker(time.Duration(syncInterval) * time.Second)

	ClockWG.Add(1)
	go func() {
		defer ClockWG.Done()
		defer func() {
			t.Stop()
		}()
		defer WatcherContextCancel()

		for {
			select {
			case <-t.C:
				// 如果只是保持失败，没必要让整个程序退出
				if err = SaveGlobalFileStatesToDiskFile(filePath); err != nil {
					k3.K3LogError("[ClockSyncGlobalFileStatesToDiskFile] save file state to disk failed: %v\n", err)
				}
				k3.K3LogDebug("[ClockSyncGlobalFileStatesToDiskFile] save file state to disk success.")
			case <-WatcherContext.Done(): // 退出协程，并退出ClockSyncGlobalFileStatesToDiskFile的定时器
				return
			}
		}
	}()

	go func() {
		ClockWG.Wait() // 阻塞等待Clock定时器协程协程退出
		k3.K3LogInfo("[ClockSyncGlobalFileStatesToDiskFile]  All clock goroutine  exit.")
		WatcherContextCancel()
	}()
}

// Run 启动监听, directory 是一个map，key是索引名称，value是索引对应的目录列表, 所有的子目录也包含
func Run(directory map[string][]string) (func(), error) {
	var (
		err error
	)
	// 初始化用到的所有全局变量
	InitVars()

	// 1. 初始化批量日志写入, 引入elk
	if err = InitConsumerBatchLog(); err != nil {
		return nil, errors.New("[Run] InitConsumerBatchLog failed: " + err.Error())
	}

	// 2. 初始化FileState 文件, state file 文件是以工作根目录为基准的相对目录
	// 2.1. 检查core.json是否存在，不存在就创建，并且load到FileState变量中
	if !k3.FileExists(FileStateFilePath) {
		// 创建文件
		if _, err = os.OpenFile(FileStateFilePath, os.O_CREATE, os.ModePerm); err != nil {
			return nil, errors.New("[Run] create state file failed: " + err.Error())
		}
	}

	// 打开文件FileStateFilePath, 并将FileStateFilePath的数据load到GlobalFileStates变量中(内存)
	if err = LoadDiskFileToGlobalFileStates(FileStateFilePath); err != nil {
		return nil, errors.New("[Run] load file state failed : " + err.Error())
	}

	// 2.2. 遍历硬盘上的所有文件，如果GlobalFileStates中没有，就add
	// 2.3. 检查GlobalFileStates中的文件是否存在，不存在就delete掉
	// 2.4. 将GlobalFileStates最新数据更新到FileStateFilePath
	if err = ScanLogFileToGlobalFileStatesAndSaveToDiskFile(directory, FileStateFilePath); err != nil {
		return nil, errors.New("[Run] scan log file state failed: " + err.Error())
	}

	// 3. 初始化watcher，每个index_name 创建一个协程来监听, 如果有协程创建不成功，或者意外退出，则程序终止
	if err = InitWatcher(directory, FileStateFilePath); err != nil {
		return Closed, err
	}

	// 4. TODO 需要检查代码 -> 定时更新 FileState 数据到硬盘
	ClockSyncGlobalFileStatesToDiskFile(FileStateFilePath)

	return Closed, nil
}

// Closed 清理协程，并关闭资源
func Closed() {
	k3.K3LogDebug("[Closed] closed watch.")
	// 回收定时器协程和监听协程
	WatcherContextCancel()
	time.Sleep(time.Second * 1) // 留1s的时间给协程来回收资源
	// 回收批量写入日志的协程
	GlobalDataAnalytics.Close()
}
