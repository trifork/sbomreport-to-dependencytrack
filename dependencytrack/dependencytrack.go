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
	UploadBOM(ctx context.Context, projectName, projectVersion string, parentName string, parentVersion string, bom []byte, isLatest bool, group string, description string) error
	AddTagsToProject(ctx context.Context, projectName, projectVersion string, tags []string, namespace string, serviceName string) error
	IsLatest(ctx context.Context, projectName, projectVersion string, updateTimestamp int64) bool
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

func (dt *DependencyTrack) UploadBOM(ctx context.Context, projectName, projectVersion string, parentName string, parentVersion string, bom []byte, isLatest bool, namespace string, serviceName string) error {
	log.Printf("Uploading BOM: project %s:%s", projectName, projectVersion)

	// Ensure project exists with correct group and description
	project, err := dt.Client.Project.Lookup(ctx, projectName, projectVersion)
	if err != nil {
		// Project does not exist, create it
		project := dtrack.Project{
			Name:    projectName,
			Version: projectVersion,
			Active:  true,
		}
		_, err = dt.Client.Project.Create(ctx, project)
		if err != nil {
			return err
		}
		// Re-lookup to get the created project's UUID and state
		project, err = dt.Client.Project.Lookup(ctx, projectName, projectVersion)
		if err != nil {
			return err
		}
	}

	// log project to be uploaded
	log.Printf("Project to be uploaded: %s:%s UUID: %s", projectName, projectVersion, project.UUID)

	uploadToken, err := dt.Client.BOM.Upload(ctx, dtrack.BOMUploadRequest{
		ProjectUUID:    &project.UUID,
		ProjectVersion: projectVersion,
		ParentName:     parentName,
		ParentVersion:  parentVersion,
		AutoCreate:     true,
		BOM:            base64.StdEncoding.EncodeToString(bom),
		IsLatest:       &isLatest,
	})
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
				processing, err := dt.Client.Event.IsBeingProcessed(ctx, dtrack.EventToken(uploadToken))
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

func (dt *DependencyTrack) IsLatest(ctx context.Context, projectName, projectVersion string, updateTimestamp int64) bool {
	project, err := dt.Client.Project.Lookup(ctx, projectName, projectVersion)
	if err != nil {
		return true
	}
	return int64(project.LastBOMImport) < updateTimestamp
}

func (dt *DependencyTrack) AddTagsToProject(ctx context.Context, projectName, projectVersion string, tags []string, namespace string, serviceName string) error {
	log.Printf("Adding tags to project. project %s:%s tags %v namespace %s serviceName %s", projectName, projectVersion, tags, namespace, serviceName)

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
