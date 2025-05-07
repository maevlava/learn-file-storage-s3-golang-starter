package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
	"io"
	"log"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"
)

type StreamInfo struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

type FFProbeOutput struct {
	Streams []StreamInfo `json:"streams"`
}

// store video to s3 tubely
func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const maxMemory = 10 << 30
	r.Body = http.MaxBytesReader(w, r.Body, maxMemory)

	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid video ID", err)
		return
	}
	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
	}
	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
	}
	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "Couldn't get video", err)
	}
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "You can't upload this video", err)
	}

	file, fileHeader, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()

	rawContentType := fileHeader.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(rawContentType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid Content-Type", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Only MP4 videos are supported", nil)
		return
	}

	//save to a temp file
	tmpFile, err := os.CreateTemp("", "video-upload-*.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create temp file", err)
		return
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	_, err = io.Copy(tmpFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to write file", err)
		return
	}

	originalTempFilePath := tmpFile.Name()

	aspectRatio, err := getVideoAspectRatio(originalTempFilePath)
	if err != nil {
		tmpFile.Close()
		respondWithError(w, http.StatusInternalServerError, "Failed to get video aspect ratio", err)
		return
	}

	processedFilePath, err := processVideoForFastStart(originalTempFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to process video for fast start", err)
		return
	}
	defer os.Remove(processedFilePath)

	randomBytes := make([]byte, 16)
	_, err = rand.Read(randomBytes)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't generate random bytes", err)
		return
	}
	randomHexFileName := hex.EncodeToString(randomBytes)
	filename := randomHexFileName + ".mp4"

	var prefix string
	switch aspectRatio {
	case "16:9":
		prefix = "landscape"
	case "9:16":
		prefix = "portrait"
	default:
		prefix = "other"
	}

	key := fmt.Sprintf("%s/%s", prefix, filename)

	processedFile, err := os.Open(processedFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to open processed file for upload", err)
		return
	}
	defer processedFile.Close()

	putObjectInput := &s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(key),
		Body:        processedFile,
		ContentType: aws.String(mediaType),
	}

	_, err = cfg.s3Client.PutObject(context.TODO(), putObjectInput)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't upload file to s3", err)
		return
	}

	cfDomain := strings.TrimSuffix(cfg.CfDistributionDomain, "/")
	publicVideoURL := fmt.Sprintf("https://%s/%s", cfDomain, key)
	video.VideoURL = &publicVideoURL

	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
	}

	log.Printf("Successfully processed and uploaded video ID %s, URL: %s\n", videoIDString, publicVideoURL)
	respondWithJSON(w, http.StatusOK, video)
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)

	var out bytes.Buffer
	cmd.Stdout = &out

	if err := cmd.Run(); err != nil {
		return "", err
	}

	var result struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
	}

	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		return "", err
	}

	if len(result.Streams) == 0 {
		return "", errors.New("no video streams found")
	}

	width := result.Streams[0].Width
	height := result.Streams[0].Height

	if width == 0 || height == 0 {
		return "other", nil
	}

	ratio := float64(width) / float64(height)

	const epsilon = 0.05

	if math.Abs(ratio-16.0/9.0) < epsilon {
		return "16:9", nil
	} else if math.Abs(ratio-9.0/16.0) < epsilon {
		return "9:16", nil
	}

	return "other", nil
}

func processVideoForFastStart(inputFilePath string) (string, error) {
	outputFilePath := inputFilePath + ".faststart"
	cmd := exec.Command("ffmpeg",
		"-i", inputFilePath,
		"-c", "copy",
		"-bsf:v", "h264_mp4toannexb",
		"-f", "mp4", "-movflags",
		"faststart", outputFilePath)

	output, err := cmd.CombinedOutput()
	if err != nil {
		os.Remove(outputFilePath)
		return "", fmt.Errorf("failed to process video for faststart: %s", output)
	}
	fmt.Printf("Successfully processed video: '%s'\n", outputFilePath)

	return outputFilePath, nil
}
