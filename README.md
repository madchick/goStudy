# goStudy

<br>

## 설치

- https://go.dev/dl/

- go version

<br>

## 샘플 테스트

- mkdir hello-go

- cd hello-go

- go mod init example/hello

- `main.go` 파일을 만들고 아래 샘플 코드를 작성

  ```go
  package main
  
  import (
      "fmt"
      "time"
  )
  
  func main() {
      fmt.Println("안녕하세요! Go 언어 첫 실행입니다.")
      fmt.Println("현재 시간:", time.Now().Format("2006-01-02 15:04:05"))
  }
  ```

- go run main.go

- go build

- go build -o myapp.exe main.go

<br>

## 윈도우에서 리눅스 빌드

- x86

  - set GOOS=linux

  - set GOARCH=amd64

- ARM

  - set GOOS=linux

  - set GOARCH=arm64

- go build -o app-linux main.go

- 리눅스 실행

  - chmod +x ./app-linux

  - ./app-linux

- 다시 윈도우 빌드할 때

  - set GOOS=windows

  - set GOARCH=amd64