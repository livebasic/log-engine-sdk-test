package watch

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/fsnotify/fsnotify"
	"io"
	"log-engine-sdk/pkg/k3"
	"log-engine-sdk/pkg/k3/config"
	"os"
	"path/filepath"
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

var (
	DefaultMaxReadCount     = 200
	DefaultObsoleteInterval = 1  // 单位天，用于解决文件已经长时间没写了，或者删除了， 但file state未处理
	DefaultSyncInterval     = 60 // 单位秒, 用于解决运行时，不断落盘

	GlobalFileStateFds       = make(map[string]*os.File)   // 对应所有要监控的文件fd
	GlobalFileStates         = make(map[string]*FileState) // 对应监控的所有文件的状态，映射 core.json文件
	GlobalWatchContextCancel context.CancelFunc
	GlobalWatchContext       context.Context // 控制协程主动退出
	GlobalWatchWG            *sync.WaitGroup
	GlobalWatchMutex         sync.Mutex // 控制全局变量的并发
	FileStateLock            sync.Mutex
)

func Run() error {

	var (
		diskPaths     = make(map[string][]string)
		diskFilePaths = make(map[string][]string)
		err           error
	)

	// 用于测试用
	config.GlobalConfig.Watch = config.Watch{
		ReadPath: map[string][]string{
			"index_nginx": []string{
				"/Users/yelei/data/code/go-projects/logs/nginx",
			},
			"index_admin": []string{
				"/Users/yelei/data/code/go-projects/logs/admin",
			},
			"index_api": []string{
				"/Users/yelei/data/code/go-projects/logs/api",
			},
		},
		StateFilePath:    "state/core.json",
		MaxReadCount:     1000,
		ObsoleteInterval: 1,
	}

	// 如果state file文件没有就创建，如果有就load文件内容到stateFile
	if GlobalFileStates, err = CreateORLoadFileState(config.GlobalConfig.Watch.StateFilePath); err != nil {
		k3.K3LogError("WatchRun CreateAndLoadFileState error: %s", err.Error())
		return err
	}

	// 遍历所有的目录,找到所有需要监控的目录(包含子目录) 和 所有文件
	for indexName, paths := range config.GlobalConfig.Watch.ReadPath {
		for _, path := range paths {
			subPaths, err := FetchWatchPath(path)
			if err != nil {
				k3.K3LogError("FetchWatchPath error: %s", err.Error())
				return err
			}
			diskPaths[indexName] = subPaths

			filePaths, err := FetchWatchPathFile(path)
			if err != nil {
				k3.K3LogError("FetchWatchPathFile error: %s", err.Error())
				return err
			}
			diskFilePaths[indexName] = filePaths
		}
	}

	fmt.Println(diskPaths, diskFilePaths, GlobalFileStates)

	/*
		watch.yaml 配置文件信息
			  read_path : # read_path每个Key的目录不可以重复，且value不可以包含相同的子集
			    index_nginx: ["/Users/yelei/data/code/go-projects/logs/nginx"] # 必须是目录
			    index_admin : [ "/Users/yelei/data/code/go-projects/logs/admin"]
			    index_api : [ "/Users/yelei/data/code/go-projects/logs/api"]
			  max_read_count : 100 # 监控到文件变化时，一次读取文件最大次数, 默认100次
			  sync_interval : 60 # 单位秒，默认60, 程序运行过程中，要定时落盘
			  state_file_path : "/state/core.json" # 记录监控文件的offset
			  obsolete_interval : 1 # 单位小时, 默认1 表示定时多久时间检查文件是否已经读完了
			  obsolete_date : 1 # 单位填， 默认1， 表示文件如果1天没有写入, 就查看下是不是读取完了，没读完就读完整个文件.
	*/
	if err = SyncFileStates2Disk(diskFilePaths, config.GlobalConfig.Watch.StateFilePath); err != nil {
		return err
	}

	// 初始化待监控的所有文件的FD
	if err = InitFileStateFds(); err != nil {
		return err
	}

	// 开始监控, 注意多协程处理，每个index name一个线程
	GlobalWatchContext, GlobalWatchContextCancel = context.WithCancel(context.Background())
	GlobalWatchWG = &sync.WaitGroup{}
	InitWatcher(diskPaths)

	ClockSyncFileState()

	if err = ClockCheckFileStateAndReadFile(); err != nil {
		return err
	}

	return nil
}

func InitWatcher(diskPaths map[string][]string) {

	// 循环开协程，每个index name 一个协程
	for index, paths := range diskPaths {
		GlobalWatchWG.Add(1)
		go doWatch(index, paths)
	}

}

func doWatch(index string, paths []string) {
	var (
		childWG = &sync.WaitGroup{}
		err     error
		watcher *fsnotify.Watcher
	)

	// 协程退出
	defer GlobalWatchWG.Done()

	// 初始化协程watcher
	if watcher, err = fsnotify.NewWatcher(); err != nil {
		GlobalWatchMutex.Lock()
		GlobalWatchContextCancel()
		GlobalWatchMutex.Unlock()
		k3.K3LogError("Failed to create watcher for %s: %v\n", index, err)
		return
	}
	defer watcher.Close()

	// 开始监听目录, 如果错误就退出
	for _, path := range paths {
		if err = watcher.Add(path); err != nil {
			GlobalWatchMutex.Lock()
			GlobalWatchContextCancel()
			GlobalWatchMutex.Unlock()
			k3.K3LogError("Failed to add %s to watcher for %s: %v\n", path, index, err)
			return
		}
	}

	childWG.Add(1)
	go func() {
		defer childWG.Done()
		defer func() {
			if r := recover(); r != nil {
				GlobalWatchMutex.Lock()
				GlobalWatchContextCancel()
				GlobalWatchMutex.Unlock()
				k3.K3LogError("doWatch child goroutine panic: %s\n", r)
			}
		}()

		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok { // 退出子协程
					return
				}

				// TODO 这里可以处理监控到文件的变化，比如文件大小变化，文件内容变化，文件删除等
				handlerEvent(event)

			case err, ok := <-watcher.Errors:
				if !ok { // 退出子协程
					return
				}
				k3.K3LogError("Child Goroutine Error reading %s: %v\n", index, err)

			case <-GlobalWatchContext.Done(): // 退出子协程
				k3.K3LogInfo("Received exit signal, child goroutine stopping....\n")
				return
			}
		}
	}()
	// 等待子协程退出
	childWG.Wait()
}

// handlerEvent 处理监控到文件的变化
func handlerEvent(event fsnotify.Event) {
	k3.K3LogInfo("handlerEvent: %v, %v, %v \n", event, event.Name, event.Op)
}

func Close() {
	// 关闭所有打开的文件
	for _, fd := range GlobalFileStateFds {
		fd.Close()
	}
	GlobalWatchContextCancel()
	GlobalWatchWG.Wait()
}

func InitFileStateFds() error {
	var (
		err error
	)

	for filePath, _ := range GlobalFileStates {
		if GlobalFileStateFds[filePath], err = os.OpenFile(filePath, os.O_RDONLY, 0666); err != nil {
			return fmt.Errorf("InitFileStateFds open file error: %s", err.Error())
		}

		if GlobalFileStates[filePath].StartReadTime == 0 {
			GlobalFileStates[filePath].StartReadTime = time.Now().Unix()
		}
	}

	return nil
}

// SyncFileStates2Disk 将FileState数据写入到磁盘, 先删除在覆盖
func SyncFileStates2Disk(diskFilePaths map[string][]string, filePath string) error {
	var (
		fd      *os.File
		err     error
		encoder *json.Encoder
	)

	SyncWatchFiles2FileStates(diskFilePaths)
	SyncFileStates2WatchFiles(diskFilePaths)

	FileStateLock.Lock()
	defer FileStateLock.Unlock()

	// 将数据写入到 state_file_path
	if fd, err = os.OpenFile(filePath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0666); err != nil {
		return fmt.Errorf("open state file error: %s", err.Error())
	}

	defer fd.Close()

	encoder = json.NewEncoder(fd)

	if err = encoder.Encode(GlobalFileStates); err != nil {
		return fmt.Errorf("encode state file error: %s", err.Error())
	}

	return nil
}

// SyncWatchFiles2FileStates 初始化时
// 遍历硬盘上被监控目录的所有文件, 判断文件是否在FileState中，如果不在，证明是新增的文件, 则添加到FileState中
func SyncWatchFiles2FileStates(watchFiles map[string][]string) {
	for index, files := range watchFiles {
		for _, diskFilePath := range files {
			if !CheckDiskFileIsExistInFileStates(diskFilePath) {
				GlobalFileStates[diskFilePath] = &FileState{
					Path:      diskFilePath,
					Offset:    0,
					IndexName: index,
				}
			}
		}
	}
}

// CheckDiskFileIsExistInFileStates 判断文件是否在FileState中
func CheckDiskFileIsExistInFileStates(diskFilePath string) bool {
	for filePath := range GlobalFileStates {
		if filePath == diskFilePath {
			return true
		}
	}
	return false
}

// SyncFileStates2WatchFiles 初始化时
// 遍历FileState中记录的所有文件，如果文件不存在于本地硬盘中，证明已经被删除了，对应在FileState中删除
func SyncFileStates2WatchFiles(watchFiles map[string][]string) {
	for fileStatePath := range GlobalFileStates {
		if !CheckFileStateIsExistInDiskFiles(fileStatePath, watchFiles) {
			delete(GlobalFileStates, fileStatePath)
		}
	}
}

// CheckFileStateIsExistInDiskFiles 判断FileState是否在硬盘中
func CheckFileStateIsExistInDiskFiles(fileStatePath string, watchFiles map[string][]string) bool {
	for _, files := range watchFiles {
		for _, diskFilePath := range files {
			if diskFilePath == fileStatePath {
				return true
			}
		}
	}
	return false
}

// CreateORLoadFileState 创建并加载状态文件
func CreateORLoadFileState(fileSatePath string) (map[string]*FileState, error) {
	var (
		fd         *os.File
		err        error
		decoder    *json.Decoder
		fileStates = make(map[string]*FileState)
	)
	// 判断文件是否存在, 不存在就创建, 存在就将文本内容加载出来,映射到SateFile中
	if fd, err = os.OpenFile(fileSatePath, os.O_CREATE|os.O_RDWR, 0666); err != nil {
		return nil, err
	}
	defer fd.Close()

	decoder = json.NewDecoder(fd)

	if err = decoder.Decode(&fileStates); err != nil && err != io.EOF {
		return nil, err
	}

	return fileStates, nil
}

// FetchWatchPath 获取需要监控的目录中的所有子目录
func FetchWatchPath(watchPath string) ([]string, error) {

	var (
		paths []string
		err   error
	)

	if err = filepath.WalkDir(watchPath, func(currentPath string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			paths = append(paths, currentPath)
		}

		return nil
	}); err != nil {
		return nil, err
	}

	return paths, err
}

// FetchWatchPathFile 获取监控目录中的所有文件
func FetchWatchPathFile(watchPath string) ([]string, error) {
	return k3.FetchDirectory(watchPath, -1)
}

// ForceSyncFileState 强制同步FileState, 先清空，在同步
func ForceSyncFileState() error {
	var (
		fd      *os.File
		err     error
		encoder *json.Encoder
	)
	FileStateLock.Lock()
	defer FileStateLock.Unlock()
	if fd, err = os.OpenFile(config.GlobalConfig.Watch.StateFilePath, os.O_RDWR|os.O_TRUNC, 0666); err != nil {
		return fmt.Errorf("open state file error: %s", err.Error())
	}

	defer fd.Close()
	encoder = json.NewEncoder(fd)

	if err = encoder.Encode(&GlobalFileStates); err != nil {
		return fmt.Errorf("encode state file error: %s", err.Error())
	}
	return nil
}

// ClockSyncFileState 定时将内存中的file state 写入硬盘
func ClockSyncFileState() {
	var (
		syncInterval = config.GlobalConfig.Watch.ObsoleteInterval
		t            *time.Ticker
	)

	// 获取监控时间间隔
	if syncInterval <= 0 || syncInterval > DefaultSyncInterval {
		syncInterval = DefaultSyncInterval
	}
	// 创建定时器
	t = time.NewTicker(time.Duration(syncInterval) * time.Second)

	GlobalWatchWG.Add(1)
	go func() {
		defer GlobalWatchWG.Done()
		defer func() {
			t.Stop()
		}()
		for {
			select {
			case <-GlobalWatchContext.Done():
				return
				// 获取到定时时间
			case <-t.C:
				// 执行同步逻辑任务
				if err := ForceSyncFileState(); err != nil {
					k3.K3LogError("ForceSyncFileState: %s\n", err)
				}
			}
		}
	}()
}

// ClockCheckFileStateAndReadFile 定时监控file states 中的文件状态，是不是长时间没有写入，如果没有就一次性读取完, 注意子协程的回收处理
func ClockCheckFileStateAndReadFile() error {

	var (
		obInterval = config.GlobalConfig.Watch.ObsoleteInterval
		t          *time.Ticker
	)

	if obInterval <= 0 || obInterval > DefaultObsoleteInterval {
		obInterval = DefaultObsoleteInterval
	}

	// 创建定时器
	t = time.NewTicker(time.Duration(obInterval) * time.Hour)

	GlobalWatchWG.Add(1)

	go func() {
		defer GlobalWatchWG.Done()

		defer func() {
			t.Stop()
		}()

		for {
			select {
			case <-GlobalWatchContext.Done():
				return
			case <-t.C:
				handleReadFileAndSendData()
			}
		}
	}()

	return nil
}

// handleReadFileAndSendData 处理读取文件并发送数据
func handleReadFileAndSendData() {
	var (
		wg = &sync.WaitGroup{}
	)

	for fileName, fileState := range GlobalFileStates {
		now := time.Now()
		lastReadTime := time.Unix(fileState.LastReadTime, 0)
		// 判断当前时间点是否超过N天未读写
		if now.Sub(lastReadTime) > 24*time.Hour*time.Duration(config.GlobalConfig.Watch.ObsoleteDate) {

			// 判断文件是否读取完，如果没有，就一次性全部读取
			if fd, ok := GlobalFileStateFds[fileName]; ok && fd != nil {
				if fstat, err := fd.Stat(); err != nil {
					k3.K3LogError("stat file error: %s", err)
					continue
				} else {
					if fstat.Size() != fileState.Offset {
						// 开协程，一次性读取所有内容
						wg.Add(1)
						go readFileFull(fd, wg)
					}
				}
			}
		}
	}

	wg.Wait()
}

func readFileFull(fd *os.File, wg *sync.WaitGroup) {
	wg.Done()

}

// ReadFileByOffset 从文件偏移量开始读取文件, 并返回当前读取的偏移量和错误信息
func readFileByOffset(fd *os.File, offset int64) (lastOffset int64, err error) {
	var (
		currentReadIndex int // 当前读取次数
		reader           *bufio.Reader
		content          string
		maxReadCount     = config.GlobalConfig.Watch.MaxReadCount
	)

	if fd != nil {
		return offset, fmt.Errorf("file descriptor is nil")
	}

	if maxReadCount <= 0 || config.GlobalConfig.Watch.MaxReadCount > DefaultMaxReadCount {
		maxReadCount = DefaultMaxReadCount
	}

	currentReadIndex = 0
	lastOffset = offset

	// 将文件偏移量偏移至指定位置
	if _, err = fd.Seek(offset, io.SeekStart); err != nil {
		return offset, fmt.Errorf("seek file error: %s", err.Error())
	}

	// 封装读取器
	reader = bufio.NewReader(fd)

	// 循环读取文件
	for currentReadIndex < maxReadCount {

		// 控制文件读取次数
		currentReadIndex++

		// 读取文件
		line, err := reader.ReadString('\n')

		// 读取文件错误, 有可能读完了
		if err != nil {
			k3.K3LogError("read file error: %s\n", err)
			break
		}

		if len(line) > 0 {
			content += line
			lastOffset += int64(len(line))
		}
	}

	if len(content) > 0 {
		if err = sendData2Consumer(content); err != nil {
			// TODO 数据读出来了，但是发送失败，需要将失败的数据存储起来, 方便后续处理
			return lastOffset, err
		}
	}

	return lastOffset, nil
}

// 将数据发送给consumer
func sendData2Consumer(content string) error {
	return nil
}
