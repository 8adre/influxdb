package http

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	http "net/http"
	"time"

	"github.com/influxdata/influxdb"
	pcontext "github.com/influxdata/influxdb/context"
	"github.com/influxdata/influxdb/kit/tracing"
	"github.com/influxdata/influxdb/predicate"
	"github.com/julienschmidt/httprouter"
	"go.uber.org/zap"
)

// DeleteBackend is all services and associated parameters required to construct
// the DeleteHandler.
type DeleteBackend struct {
	Logger *zap.Logger
	influxdb.HTTPErrorHandler

	DeleteService       influxdb.DeleteService
	BucketService       influxdb.BucketService
	OrganizationService influxdb.OrganizationService
}

// NewDeleteBackend returns a new instance of DeleteBackend
func NewDeleteBackend(b *APIBackend) *DeleteBackend {
	return &DeleteBackend{
		Logger: b.Logger.With(zap.String("handler", "delete")),

		HTTPErrorHandler:    b.HTTPErrorHandler,
		DeleteService:       b.DeleteService,
		BucketService:       b.BucketService,
		OrganizationService: b.OrganizationService,
	}
}

// DeleteHandler receives a delete request with a predicate and sends it to storage.
type DeleteHandler struct {
	influxdb.HTTPErrorHandler
	*httprouter.Router

	Logger *zap.Logger

	DeleteService       influxdb.DeleteService
	BucketService       influxdb.BucketService
	OrganizationService influxdb.OrganizationService
}

const (
	deletePath = "/api/v2/delete"
)

// NewDeleteHandler creates a new handler at /api/v2/delete to recieve delete requests.
func NewDeleteHandler(b *DeleteBackend) *DeleteHandler {
	h := &DeleteHandler{
		HTTPErrorHandler: b.HTTPErrorHandler,
		Router:           NewRouter(b.HTTPErrorHandler),
		Logger:           b.Logger,

		BucketService:       b.BucketService,
		DeleteService:       b.DeleteService,
		OrganizationService: b.OrganizationService,
	}

	h.HandlerFunc("POST", deletePath, h.handleDelete)
	return h
}

func (h *DeleteHandler) handleDelete(w http.ResponseWriter, r *http.Request) {
	span, r := tracing.ExtractFromHTTPRequest(r, "DeleteHandler")
	defer span.Finish()

	ctx := r.Context()
	defer r.Body.Close()

	a, err := pcontext.GetAuthorizer(ctx)
	if err != nil {
		h.HandleHTTPError(ctx, err, w)
		return
	}

	dr, err := decodeDeleteRequest(
		ctx, r,
		h.OrganizationService,
		h.BucketService,
	)
	if err != nil {
		h.HandleHTTPError(ctx, err, w)
		return
	}

	p, err := influxdb.NewPermissionAtID(dr.Bucket.ID, influxdb.WriteAction, influxdb.BucketsResourceType, dr.Org.ID)
	if err != nil {
		h.HandleHTTPError(ctx, &influxdb.Error{
			Code: influxdb.EInternal,
			Op:   "http/handleDelete",
			Msg:  fmt.Sprintf("unable to create permission for bucket: %v", err),
			Err:  err,
		}, w)
		return
	}

	if !a.Allowed(*p) {
		h.HandleHTTPError(ctx, &influxdb.Error{
			Code: influxdb.EForbidden,
			Op:   "http/handleDelete",
			Msg:  "insufficient permissions for write",
		}, w)
		return
	}

	// send delete points request to storage
	err = h.DeleteService.DeleteBucketRangePredicate(ctx,
		dr.Org.ID,
		dr.Bucket.ID,
		dr.Start,
		dr.Stop,
		dr.Predicate,
	)
	if err != nil {
		h.HandleHTTPError(ctx, err, w)
		return
	}
	h.Logger.Debug("deleted",
		zap.String("orgID", fmt.Sprint(dr.Org.ID.String())),
		zap.String("buketID", fmt.Sprint(dr.Bucket.ID.String())),
	)

	w.WriteHeader(http.StatusNoContent)
}

func decodeDeleteRequest(ctx context.Context,
	r *http.Request,
	orgSvc influxdb.OrganizationService,
	bucketSvc influxdb.BucketService,
) (*deleteRequest, error) {
	dr := new(deleteRequest)
	err := json.NewDecoder(r.Body).Decode(dr)
	if err != nil {
		return nil, &influxdb.Error{
			Code: influxdb.EInvalid,
			Msg:  "invalid request; error parsing request json",
			Err:  err,
		}
	}
	dr.Org, err = queryOrganization(ctx, r, orgSvc)
	if err != nil {
		return nil, err
	}

	dr.Bucket, err = queryBucket(ctx, r, bucketSvc)
	if err != nil {
		return nil, err
	}
	return dr, nil
}

type deleteRequest struct {
	Org       *influxdb.Organization
	Bucket    *influxdb.Bucket
	Start     int64
	Stop      int64
	Predicate influxdb.Predicate
}

type deleteRequestDecode struct {
	Start     string          `json:"start"`
	Stop      string          `json:"stop"`
	Predicate json.RawMessage `json:"predicate"`
}

func (dr *deleteRequest) UnmarshalJSON(b []byte) error {
	var drd deleteRequestDecode
	err := json.Unmarshal(b, &drd)
	if err != nil {
		return &influxdb.Error{
			Code: influxdb.EInvalid,
			Msg:  "Invalid delete predicate node request",
			Err:  err,
		}
	}
	*dr = deleteRequest{}
	if drd.Start == "" {
		dr.Start = math.MinInt64
	} else {
		start, err := time.Parse(time.RFC3339, drd.Start)
		if err != nil {
			return &influxdb.Error{
				Code: influxdb.EInvalid,
				Op:   "http/Delete",
				Msg:  "invalid RFC3339 for field start",
			}
		}
		dr.Start = start.UnixNano()
	}

	if drd.Stop == "" {
		dr.Stop = math.MaxInt64
	} else {
		stop, err := time.Parse(time.RFC3339, drd.Start)
		if err != nil {
			return &influxdb.Error{
				Code: influxdb.EInvalid,
				Op:   "http/Delete",
				Msg:  "invalid RFC3339 for field stop",
			}
		}
		dr.Stop = stop.UnixNano()
	}
	node, err := predicate.UnmarshalJSON(drd.Predicate)
	if err != nil {
		return err
	}
	dr.Predicate, err = predicate.New(node)

	return err
}