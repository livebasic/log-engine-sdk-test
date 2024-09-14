package main

import (
	"encoding/json"
	"fmt"
	"log-engine-sdk/pkg/k3"
	"log-engine-sdk/pkg/k3/config"
	"log-engine-sdk/pkg/k3/watch"
	"os"
)

func main() {
	var (
		dir     string
		err     error
		configs []string
	)
	// 初始化配置文件, 必须通过make运行
	if dir, err = os.Getwd(); err != nil {
		k3.K3LogError("get current dir error: %s", err)
		return
	}

	// 获取configs文件目录所有文件
	if configs, err = k3.FetchDirectory(dir+"/configs", -1); err != nil {
		k3.K3LogError("fetch directory error: %s", err)
	}
	config.MustLoad(configs...)

	if config.GlobalConfig.System.PrintEnabled == true {
		if configJson, err := json.Marshal(config.GlobalConfig); err != nil {
			k3.K3LogError("json marshal error: %s", err)
			return
		} else {
			fmt.Println(string(configJson))
		}
	}

	var (
		ReadDirectory []string
	)

	for _, readDir := range config.GlobalConfig.System.ReadPath {
		ReadDirectory = append(ReadDirectory, dir+readDir)
	}

	err = watch.Run(ReadDirectory, dir+config.GlobalConfig.System.StateFilePath)

	if err != nil {
		k3.K3LogError("watch error: %s", err)
	}
}
