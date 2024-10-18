package watch

import (
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
	StartReadTime time.Time
	LastReadTime  time.Time
	IndexName     string
}

var (
	GlobalFileStateFds                           = make(map[string]*os.File)
	GlobalFileStates                             = make(map[string]*FileState)
	GlobalWatchContext, GlobalWatchContextCancel = context.WithCancel(context.Background())
	GlobalWatchWG                                = &sync.WaitGroup{}
	FileStateLock                                sync.Mutex
)

func Run() error {

	var (
		watchConfig   = config.GlobalConfig.Watch
		diskPaths     = make(map[string][]string)
		diskFilePaths = make(map[string][]string)
		err           error
	)

	// 用于测试用
	watchConfig = config.Watch{
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
	if GlobalFileStates, err = CreateORLoadFileState(watchConfig.StateFilePath); err != nil {
		k3.K3LogError("WatchRun CreateAndLoadFileState error: %s", err.Error())
		return err
	}

	// 遍历所有的目录,找到所有需要监控的目录(包含子目录) 和 所有文件
	for indexName, paths := range watchConfig.ReadPath {
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
		max_read_count : 100 # 监控到文件变化时，一次读取文件最大次数
		start_date : "2020-01-01 00:00:00" # 监控什么时间起创建的文件
		obsolete_date_interval : 1 # 单位小时hour, 默认1小时, 超过多少时间文件未变化, 认为文件应该删除
		state_file_path : "/state/core
	*/
	if err = SyncFileStates2Disk(diskFilePaths, watchConfig.StateFilePath); err != nil {
		return err
	}

	// 初始化待监控的所有文件的FD
	if err = InitFileStateFds(); err != nil {
		return err
	}

	// 开始监控, 注意多协程处理，每个index name一个线程
	InitWatcher(diskPaths)

	return nil
}

func InitWatcher(diskPaths map[string][]string) {
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
		k3.K3LogError("Failed to create watcher for %s: %v\n", index, err)
		return
	}
	defer watcher.Close()

	// 开始监听目录, 如果错误就退出
	for _, path := range paths {
		if err = watcher.Add(path); err != nil {
			k3.K3LogError("Failed to add %s to watcher for %s: %v\n", path, index, err)
			// TODO 因为是循环多个目录，有可能某些目录出现问题，那么需要给外部发一个chan， 这样方便解决程序整体退出的问题
			return
		}
	}

	childWG.Add(1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
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

			case <-GlobalWatchContext.Done():
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

		if GlobalFileStates[filePath].StartReadTime.IsZero() {
			GlobalFileStates[filePath].StartReadTime = time.Now()
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

// TODO 启动后，定时检查FileState中的记录文件，如果一段时间都没有变化，证明文件不会再写入了， 就检查是否已经读完, 没读完就一次性读完它

// TODO 启动后，定时检查FileState中的记录文件，是否还存在在硬盘中，如果不存在就更新FileState

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
