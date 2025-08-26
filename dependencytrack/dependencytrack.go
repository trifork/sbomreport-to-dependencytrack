package dependencytrack

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"time"

	dtrack "github.com/DependencyTrack/client-go"
)

type DependencyTrackClient interface {
	UploadBOM(ctx context.Context, projectName, projectVersion string, parentName string, parentVersion string, bom []byte, createTimestamp string) error
	AddTagsToProject(ctx context.Context, projectName, projectVersion string, tags []string) error
}

type DependencyTrack struct {
	Client *dtrack.Client

	SBOMUploadTimeout       time.Duration
	SBOMUploadCheckInterval time.Duration
}

func New(baseURL, apiKey string, dtrackClientTimeout, sbomUploadTimeout, sbomUploadCheckInterval time.Duration) (*DependencyTrack, error) {
	client, err := dtrack.NewClient(baseURL, dtrack.WithAPIKey(apiKey), dtrack.WithTimeout(dtrackClientTimeout))
	if err != nil {
		return nil, err
	}

	return &DependencyTrack{
		Client:                  client,
		SBOMUploadTimeout:       sbomUploadTimeout,
		SBOMUploadCheckInterval: sbomUploadCheckInterval,
	}, nil
}

func (dt *DependencyTrack) UploadBOM(ctx context.Context, projectName, projectVersion string, parentName string, parentVersion string, bom []byte, createTimestamp string) error {
	log.Printf("Uploading BOM: project %s:%s", projectName, projectVersion)

	ts := time.Now().Format(time.RFC3339)

	// latest := true
	latest, err := dt.IsLatest(ctx, projectName, projectVersion, ts)
	if err != nil {
		return err
	}

	uploadToken, err := dt.Client.BOM.Upload(ctx, dtrack.BOMUploadRequest{
		ProjectName:    projectName,
		ProjectVersion: projectVersion,
		ParentName:     parentName,
		ParentVersion:  parentVersion,
		AutoCreate:     true,
		BOM:            base64.StdEncoding.EncodeToString(bom),
		IsLatest:       &latest,
	})
	log.Printf("Upload BOM: latest: %v", latest)
	if err != nil {
		return err
	}

	log.Printf("Polling completion of upload BOM: project %s:%s token %s", projectName, projectVersion, uploadToken)

	doneChan := make(chan struct{})
	errChan := make(chan error)

	go func(ctx context.Context) {
		defer func() {
			close(doneChan)
			close(errChan)
		}()

		ticker := time.NewTicker(dt.SBOMUploadCheckInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				processing, err := dt.Client.BOM.IsBeingProcessed(ctx, uploadToken)
				if err != nil {
					errChan <- err
					return
				}
				if !processing {
					doneChan <- struct{}{}
					return
				}
			case <-time.After(dt.SBOMUploadTimeout):
				errChan <- fmt.Errorf("timeout exceeded")
				return
			case <-ctx.Done():
				errChan <- ctx.Err()
				return
			}
		}
	}(ctx)

	select {
	case <-doneChan:
		log.Printf("BOM upload completed: project %s:%s token %s", projectName, projectVersion, uploadToken)
		break
	case err := <-errChan:
		log.Printf("Error: BOM upload failed: project %s:%s token %s: %s", projectName, projectVersion, uploadToken, err)
		return err
	}

	return nil
}

func (dt *DependencyTrack) AddTagsToProject(ctx context.Context, projectName, projectVersion string, tags []string) error {
	log.Printf("Adding tags to project. project %s:%s tags %v", projectName, projectVersion, tags)

	project, err := dt.Client.Project.Lookup(ctx, projectName, projectVersion)
	if err != nil {
		return err
	}

	for _, tag := range tags {
		project.Tags = append(project.Tags, dtrack.Tag{Name: tag})
	}

	_, err = dt.Client.Project.Update(ctx, project)
	if err != nil {
		return err
	}

	return nil
}

func (dt *DependencyTrack) IsLatest(ctx context.Context, projectName, projectVersion string, creationTimestamp string) (bool, error) {
	log.Printf("Checking if the BOM is the latest. project %s:%s creationTimestamp %s", projectName, projectVersion, creationTimestamp)
	// Parse creationTimestamp from JSON
	ts, err := time.Parse(time.RFC3339, creationTimestamp)
	if err != nil {
		return false, fmt.Errorf("invalid creationTimestamp: %w", err)
	}

	// Fetch the latest project info from Dependency-Track
	project, err := dt.Client.Project.Latest(ctx, projectName)
	if err != nil {
		log.Printf("No existing project found, treating as latest: %v", err)
		// If no latest exists, treat this version as latest
		return true, nil
	}

	// If the version matches the latest, it’s obviously latest
	if project.Version == projectVersion {
		log.Printf("Project version: %v matches the latest version in Dependency-Track.", projectVersion)
		return true, nil
	}

	// Compare timestamps: LastBOMImport is in milliseconds
	latestBOMTime := time.UnixMilli(int64(project.LastBOMImport))
	log.Printf("LatestBOMTime: %v, Incoming BOM creationTimestamp: %v", latestBOMTime, ts)

	// If the incoming BOM is newer than the stored latest, mark it as latest
	if ts.After(latestBOMTime) {
		log.Printf("Incoming BOM creationTimestamp %v is newer than the latest in Dependency-Track %v.", ts, latestBOMTime)
		return true, nil
	}

	log.Printf("Did not hit any conditions, treating as not latest.")
	return false, nil
}
