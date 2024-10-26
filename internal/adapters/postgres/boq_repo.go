package postgres

import (
	"boonkosang/internal/domain/models"
	"boonkosang/internal/repositories"
	"boonkosang/internal/requests"
	"boonkosang/internal/responses"
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

type boqRepository struct {
	db *sqlx.DB
}

func NewBOQRepository(db *sqlx.DB) repositories.BOQRepository {
	return &boqRepository{
		db: db,
	}
}

func (r *boqRepository) GetBoqWithProject(ctx context.Context, projectID uuid.UUID) (*responses.BOQResponse, error) {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	var data struct {
		BoqID              uuid.UUID        `db:"boq_id"`
		ProjectID          uuid.UUID        `db:"project_id"`
		BOQStatus          models.BOQStatus `db:"boq_status"`
		SellingGeneralCost sql.NullFloat64  `db:"selling_general_cost"`
	}

	boqQuery := `
        SELECT 
            b.boq_id,
            b.project_id,
            b.status as boq_status,
            b.selling_general_cost
        FROM boq b
        JOIN project p ON p.project_id = b.project_id
        WHERE b.project_id = $1`

	err = tx.GetContext(ctx, &data, boqQuery, projectID)
	if err != nil {
		if err == sql.ErrNoRows {
			// Create new BOQ if it doesn't exist
			createBOQQuery := `
                INSERT INTO boq (project_id, status, selling_general_cost) 
                VALUES ($1, 'draft', NULL) 
                RETURNING boq_id, project_id, status as boq_status, selling_general_cost`

			err = tx.GetContext(ctx, &data, createBOQQuery, projectID)
			if err != nil {
				return nil, fmt.Errorf("failed to create BOQ: %w", err)
			}
		} else {
			return nil, fmt.Errorf("failed to check BOQ existence: %w", err)
		}
	}

	// Convert to response struct
	response := &responses.BOQResponse{
		ID:                 data.BoqID,
		ProjectID:          data.ProjectID,
		Status:             data.BOQStatus,
		SellingGeneralCost: data.SellingGeneralCost.Float64,
	}

	fmt.Print(response)

	jobsQuery := `
   SELECT DISTINCT
	j.*
FROM job j
JOIN boq_job bj ON j.job_id = bj.job_id
WHERE bj.boq_id = $1
`

	var jobs []models.Job
	err = tx.SelectContext(ctx, &jobs, jobsQuery, data.BoqID)
	if err != nil {
		return nil, fmt.Errorf("failed to get jobs: %w", err)
	}

	var jobForResponse []responses.JobResponse
	for _, job := range jobs {
		jobForResponse = append(jobForResponse, responses.JobResponse{
			JobID:       job.JobID,
			Name:        job.Name,
			Description: job.Description.String,
			Unit:        job.Unit,
		})
	}

	response.Jobs = jobForResponse

	return response, nil
}

func (r *boqRepository) AddBOQJob(ctx context.Context, boqID uuid.UUID, req requests.BOQJobRequest) error {
	// Start transaction
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Check BOQ status
	var status string
	checkStatusQuery := `SELECT status FROM boq WHERE boq_id = $1`
	err = tx.GetContext(ctx, &status, checkStatusQuery, boqID)
	if err != nil {
		if err == sql.ErrNoRows {
			return errors.New("boq not found")
		}
		return fmt.Errorf("failed to get BOQ status: %w", err)
	}

	if status != "draft" {
		return errors.New("can only add jobs to BOQ in draft status")
	}

	// 16.2 Insert into boq_job
	insertBOQJobQuery := `
        INSERT INTO boq_job (
            boq_id, job_id, quantity, labor_cost
        ) VALUES (
            $1, $2, $3, $4
        )`

	_, err = tx.ExecContext(ctx, insertBOQJobQuery,
		boqID,
		req.JobID,
		req.Quantity,
		req.LaborCost,
	)
	if err != nil {
		return fmt.Errorf("failed to add job to BOQ: %w", err)
	}

	// 16.3 Get materials for the job and add to material_price_log if not exists
	materialQuery := `
        SELECT material_id, quantity 
        FROM job_material 
        WHERE job_id = $1`

	type JobMaterial struct {
		MaterialID string  `db:"material_id"`
		Quantity   float64 `db:"quantity"`
	}
	var materials []JobMaterial

	err = tx.SelectContext(ctx, &materials, materialQuery, req.JobID)
	if err != nil {
		return fmt.Errorf("failed to get job materials: %w", err)
	}

	// For each material, check if it exists in material_price_log
	for _, material := range materials {
		// Check if material price log exists
		var exists bool
		checkExistsQuery := `
            SELECT EXISTS(
                SELECT 1 
                FROM material_price_log 
                WHERE boq_id = $1 
                AND material_id = $2 
                AND job_id = $3
            )`

		err = tx.GetContext(ctx, &exists, checkExistsQuery, boqID, material.MaterialID, req.JobID)
		if err != nil {
			return fmt.Errorf("failed to check material price log existence: %w", err)
		}

		if !exists {
			insertPriceLogQuery := `
                INSERT INTO material_price_log (
                    material_id, boq_id, job_id, quantity, updated_at
                ) VALUES (
                    $1, $2, $3, $4, CURRENT_TIMESTAMP
                )`

			_, err = tx.ExecContext(ctx, insertPriceLogQuery,
				material.MaterialID,
				boqID,
				req.JobID,
				material.Quantity,
			)
			if err != nil {
				return fmt.Errorf("failed to create material price log: %w", err)
			}
		}
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}

func (r *boqRepository) DeleteBOQJob(ctx context.Context, boqID uuid.UUID, jobID uuid.UUID) error {
	// Start transaction
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Check BOQ status
	var status string
	checkStatusQuery := `SELECT status FROM boq WHERE boq_id = $1`
	err = tx.GetContext(ctx, &status, checkStatusQuery, boqID)
	if err != nil {
		if err == sql.ErrNoRows {
			return errors.New("boq not found")
		}
		return fmt.Errorf("failed to get BOQ status: %w", err)
	}

	if status != "draft" {
		return errors.New("can only delete jobs from BOQ in draft status")
	}

	deleteQuery := `
		DELETE FROM boq_job 
		WHERE boq_id = $1 
		AND job_id = $2`

	_, err = tx.ExecContext(ctx, deleteQuery, boqID, jobID)
	if err != nil {
		return fmt.Errorf("failed to delete job from BOQ: %w", err)
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil

}
