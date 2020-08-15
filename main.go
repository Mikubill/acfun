package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cheggaaa/pb/v3"
)

var (
	token = flag.String("token", "", "Your User Token (a.k.a acPasstoken)")
	uid   = flag.String("uid", "", "Your User ID (a.k.a auth_key)")
	debug = flag.Bool("verbose", false, "Verbose Mode")
	auth  string
)

const (
	UploadConfig   = "https://member.acfun.cn/video/api/getKSCloudToken"
	UploadFinish   = "https://member.acfun.cn/video/api/uploadFinish"
	CreateVideo    = "https://member.acfun.cn/video/api/createVideo"
	UploadResume   = "https://mediacloud.kuaishou.com/api/upload/resume"
	UploadEndpoint = "https://mediacloud.kuaishou.com/api/upload/fragment"
	UploadComplete = "https://mediacloud.kuaishou.com/api/upload/complete"
)

type UploadConfigResp struct {
	Result int               `json:"result"`
	Host   string            `json:"host-name"`
	Config UploadConfigBlock `json:"uploadConfig"`
	TaskID string            `json:"taskId"`
	Token  string            `json:"token"`
}

type UploadConfigBlock struct {
	PartSize             int `json:"partSize"`
	Parallel             int `json:"parallel"`
	RetryCount           int `json:"retryCount"`
	RetryDurationSeconds int `json:"retryDurationSeconds"`
}

type UploadPart struct {
	content []byte
	count   int64
}

type UploadPartResult struct {
	Result   int    `json:"result"`
	Checksum string `json:"checksum"`
	Size     int64  `json:"size"`
}

func main() {
	flag.Parse()
	files := flag.Args()

	if *debug {
		log.Printf("acPasstoken = %s", *token)
		log.Printf("auth_key = %s", *uid)
		log.Printf("verbose = true")
		log.Printf("files = %s", files)
	}
	if *token == "" || *uid == "" {
		fmt.Println("token or uid is missing")
		printUsage()
		return
	}
	auth = fmt.Sprintf("acPasstoken=%s; auth_key=%s; ", *token, *uid)

	for _, v := range files {
		fmt.Printf("Local: %s\n", v)
		if *debug {
			log.Println("retrieving file info...")
		}
		info, err := getFileInfo(v)
		if err != nil {
			fmt.Printf("getFileInfo returns error: %v", err)
			continue
		}

		config, err := getUploadConfig(info)
		if err != nil {
			fmt.Printf("getUploadConfig returns error: %v", err)
			continue
		}

		resumeURL := fmt.Sprintf("%s?upload_token=%s", UploadResume, config.Token)
		err = uploadRequest("GET", resumeURL)
		if err != nil {
			fmt.Printf("uploadRequest returns error: %v", err)
			continue
		}

		bar := pb.Full.Start64(info.Size())
		bar.Set(pb.Bytes, true)
		file, err := os.Open(v)
		if err != nil {
			fmt.Printf("openFile returns error: %v", err)
			continue
		}

		wg := new(sync.WaitGroup)
		ch := make(chan *UploadPart)
		for i := 0; i < config.Config.Parallel; i++ {
			go uploader(config.Token, config.Config.PartSize-1, info.Size(), &ch, wg, bar)
		}

		part := int64(-1)
		for {
			part++
			buf := make([]byte, config.Config.PartSize-1)
			nr, err := file.Read(buf[:])
			if nr <= 0 || err != nil {
				break
			}
			if nr > 0 {
				wg.Add(1)
				ch <- &UploadPart{
					content: buf[:nr],
					count:   part,
				}
			}
		}

		wg.Wait()
		close(ch)
		_ = file.Close()
		bar.Finish()

		if *debug {
			log.Printf("total number of fragment parts: %d", part)
		}
		// finish upload
		err = finishUpload(config.Token, part, config.TaskID, path.Base(v))
		if err != nil {
			fmt.Printf("finishUpload returns error: %v", err)
			continue
		}
	}
}

func printUsage() {
	fmt.Printf("Usage of %s:\n", os.Args[0])
	flag.PrintDefaults()
}

func uploader(token string, partSize int, fileSize int64, ch *chan *UploadPart, wg *sync.WaitGroup, bar *pb.ProgressBar) {
	for item := range *ch {
		if *debug {
			log.Printf("part %d start uploading", item.count)
		}
		client := http.Client{Timeout: 10 * time.Second}
		data := new(bytes.Buffer)
		data.Write(item.content)
		postURL := fmt.Sprintf("%s?upload_token=%s&fragment_id=%d", UploadEndpoint, token, item.count)
		req, err := http.NewRequest("POST", postURL, data)
		req.Header.Set("Content-Type", "application/octet-stream")
		start := item.count * int64(partSize)
		contentRange := fmt.Sprintf("bytes %d-%d/%d", start, start+int64(len(item.content))-1, fileSize)
		req.Header.Set("Content-Range", contentRange)
		if *debug {
			log.Println(req.Header)
		}
		resp, err := client.Do(req)
		if err != nil {
			if *debug {
				log.Printf("failed uploading part %d error: %v (retring)", item.count, err)
			}
			go func() {
				*ch <- item
			}()
			continue
		}
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			if *debug {
				log.Printf("failed reading upload part %d response error: %v (retring)", item.count, err)
			}
			go func() {
				*ch <- item
			}()
			_ = resp.Body.Close()
			continue
		}
		if *debug {
			log.Printf("upload part %d finished. Result: %s", item.count, string(body))
		}
		result := new(UploadPartResult)
		err = json.Unmarshal(body, result)
		if err != nil {
			if *debug {
				log.Printf("failed unmarshaling upload part %d response to json error: %v (retring)", item.count, err)
			}
			go func() {
				*ch <- item
			}()
			_ = resp.Body.Close()
			continue
		}
		if result.Result != 1 || result.Size != int64(len(item.content)) {
			if *debug {
				log.Printf("failed uploading part %d response: %+v (retring)", item.count, *result)
			}
			go func() {
				*ch <- item
			}()
			_ = resp.Body.Close()
			continue
		}
		_ = resp.Body.Close()
		bar.Add(len(item.content))
		wg.Done()
	}
}

func finishUpload(token string, part int64, task string, filename string) error {
	if *debug {
		log.Println("finishing upload...")
		log.Println("step1 -> api/uploadComplete")
	}
	completeURL := fmt.Sprintf("%s?fragment_count=%d&upload_token=%s", UploadComplete, part, token)
	err := uploadRequest("POST", completeURL)
	if err != nil {
		log.Printf("uploadRequest returns error: %v", err)
		return err
	}

	if *debug {
		log.Println("step2 -> api/createVideo")
	}
	data := url.Values{
		"videoKey": []string{task},
		"fileName": []string{filename},
		"vodType":  []string{"ksCloud"},
	}
	if *debug {
		log.Printf("postBody: %v", data.Encode())
		log.Printf("endpoint: %s", CreateVideo)
	}
	_, err = request(CreateVideo, data.Encode())
	if err != nil {
		return err
	}

	if *debug {
		log.Println("step3 -> api/uploadFinish")
	}
	data = url.Values{"taskId": []string{task}}
	if *debug {
		log.Printf("postBody: %v", data.Encode())
		log.Printf("endpoint: %s", UploadFinish)
	}
	_, err = request(UploadFinish, data.Encode())
	if err != nil {
		return err
	}

	return nil
}

func getUploadConfig(info os.FileInfo) (*UploadConfigResp, error) {

	if *debug {
		log.Println("retrieving upload config...")
	}
	data := url.Values{
		"fileName": []string{info.Name()},
		"size":     []string{strconv.FormatInt(info.Size(), 10)},
		"template": []string{"1"},
	}
	body, err := request(UploadConfig, data.Encode())
	if err != nil {
		return nil, err
	}
	config := new(UploadConfigResp)
	err = json.Unmarshal(body, config)
	if err != nil {
		return nil, err
	}
	return config, nil
}

func request(link string, postBody string) ([]byte, error) {
	if *debug {
		log.Printf("postBody: %v", postBody)
		log.Printf("endpoint: %s", link)
	}
	client := http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("POST", link, strings.NewReader(postBody))
	if err != nil {
		if *debug {
			log.Printf("build request returns error: %v", err)
		}
		return nil, err
	}
	req.Header.Set("authority", "member.acfun.cn")
	req.Header.Set("host", "member.acfun.cn:443")
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("accept", "application/json, text/plain, */*")
	req.Header.Set("origin", "https://member.acfun.cn")
	req.Header.Set("user-agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_3) "+
		"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/80.0.3987.149 Safari/537.36")
	req.Header.Set("referer", "https://member.acfun.cn/upload-video")
	req.Header.Set("cookie", auth)
	if *debug {
		log.Println(req.Header)
	}
	resp, err := client.Do(req)
	if err != nil {
		if *debug {
			log.Printf("do request returns error: %v", err)
		}
		return nil, err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		if *debug {
			log.Printf("read response returns: %v", err)
		}
		return nil, err
	}
	if *debug {
		log.Printf("returns: %v", string(body))
	}
	return body, nil
}

func getFileInfo(path string) (os.FileInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	return info, nil
}

func uploadRequest(method string, link string) error {
	client := http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(method, link, nil)
	if err != nil {
		if *debug {
			log.Printf("upload request returns err: %v", err)
		}
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		if *debug {
			log.Printf("response of upload request returns err: %v", err)
		}
		return err
	}
	defer resp.Body.Close()
	if *debug {
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			log.Printf("read response of upload request returns err: %v", err)
			return err
		}
		log.Printf("upload request response: %s", string(body))
	}
	return nil
}
