package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
	"io"
	"mime"
	"net/http"
	"os"
)

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

	// Reset file pointer
	if _, err = tmpFile.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "error seeking file", http.StatusInternalServerError)
		return
	}

	exts, err := mime.ExtensionsByType(mediaType)
	if err != nil || len(exts) == 0 {
		respondWithError(w, http.StatusBadRequest, "Unsupported Content-Type", err)
	}
	ext := exts[0]

	randomBytes := make([]byte, 16)
	_, err = rand.Read(randomBytes)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't generate random bytes", err)
		return
	}
	randomHexFileName := hex.EncodeToString(randomBytes)
	filename := randomHexFileName + ext

	//upload to s3
	putObjectInput := &s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(filename),
		Body:        tmpFile,
		ContentType: aws.String(mediaType),
	}
	_, err = cfg.s3Client.PutObject(context.TODO(), putObjectInput)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't upload file to s3", err)
		return
	}
	publicUrl := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, filename)
	video.VideoURL = &publicUrl
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
	}
	respondWithJSON(w, http.StatusOK, video)
}
