package resource

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/grafana/grafana/pkg/apimachinery/utils"
	"github.com/grafana/grafana/pkg/infra/appcontext"
	"github.com/grafana/grafana/pkg/services/auth/identity"
)

type WriteEvent struct {
	EventID    int64
	Key        *Key // the request key
	Requester  identity.Requester
	Operation  ResourceOperation
	PreviousRV int64 // only for Update+Delete
	Value      []byte

	Object    utils.GrafanaMetaAccessor
	OldObject utils.GrafanaMetaAccessor

	// Change metadata
	FolderChanged bool

	// The status will be populated for any error
	Status *StatusResult
	Error  error
}

func (e *WriteEvent) BadRequest(err error, message string, a ...any) *WriteEvent {
	e.Error = err
	e.Status = &StatusResult{
		Status:  "Failure",
		Message: fmt.Sprintf(message, a...),
		Code:    http.StatusBadRequest,
	}
	return e
}

// Verify that all required fields are set, and the user has permission to set the common metadata fields
type EventValidator interface {
	PrepareCreate(ctx context.Context, req *CreateRequest) (*WriteEvent, error)
	PrepareUpdate(ctx context.Context, req *UpdateRequest, current []byte) (*WriteEvent, error)
}

type EventValidatorOptions struct {
	// Get the next EventID
	NextEventID func() int64

	// Check if a user has access to write folders
	// When this is nil, no resources can have folders configured
	FolderAccess func(ctx context.Context, user identity.Requester, uid string) bool

	// When configured, this will make sure a user is allowed to save to a given origin
	OriginAccess func(ctx context.Context, user identity.Requester, origin string) bool
}

type eventValidator struct {
	opts EventValidatorOptions
}

func NewEventValidator(opts EventValidatorOptions) EventValidator {
	if opts.NextEventID == nil {
		counter := atomic.Int64{}
		opts.NextEventID = func() int64 {
			return counter.Add(1)
		}
	}
	return &eventValidator{opts}
}

type dummyObject struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
}

var _ EventValidator = &eventValidator{}

func (v *eventValidator) newEvent(ctx context.Context, key *Key, value, oldValue []byte) *WriteEvent {
	var err error
	event := &WriteEvent{
		EventID: v.opts.NextEventID(),
		Key:     key,
		Value:   value,
	}
	event.Requester, err = appcontext.User(ctx)
	if err != nil {
		return event.BadRequest(err, "unable to get user")
	}

	dummy := &dummyObject{}
	err = json.Unmarshal(value, dummy)
	if err != nil {
		return event.BadRequest(err, "error reading json")
	}

	obj, err := utils.MetaAccessor(dummy)
	if err != nil {
		return event.BadRequest(err, "invalid object in json")
	}
	if obj.GetUID() == "" {
		return event.BadRequest(nil, "the UID must be set")
	}
	if obj.GetGenerateName() != "" {
		return event.BadRequest(nil, "can not save value with generate name")
	}
	if obj.GetKind() == "" {
		return event.BadRequest(nil, "expecting resources with a kind in the body")
	}
	if obj.GetName() != key.Name {
		return event.BadRequest(nil, "key name does not match the name in the body")
	}
	if obj.GetNamespace() != key.Namespace {
		return event.BadRequest(nil, "key namespace does not match the namespace in the body")
	}
	folder := obj.GetFolder()
	if folder != "" {
		if v.opts.FolderAccess == nil {
			return event.BadRequest(err, "folders are not supported")
		} else if !v.opts.FolderAccess(ctx, event.Requester, folder) {
			return event.BadRequest(err, "unable to add resource to folder") // 403?
		}
	}
	origin, err := obj.GetOriginInfo()
	if err != nil {
		return event.BadRequest(err, "invalid origin info")
	}
	if origin != nil && v.opts.OriginAccess != nil {
		if !v.opts.OriginAccess(ctx, event.Requester, origin.Name) {
			return event.BadRequest(err, "not allowed to write resource to origin (%s)", origin.Name)
		}
	}
	event.Object = obj

	// This is an update
	if oldValue != nil {
		dummy := &dummyObject{}
		err = json.Unmarshal(oldValue, dummy)
		if err != nil {
			return event.BadRequest(err, "error reading old json value")
		}
		old, err := utils.MetaAccessor(dummy)
		if err != nil {
			return event.BadRequest(err, "invalid object inside old json")
		}
		if key.Name != old.GetName() {
			return event.BadRequest(err, "the old value has a different name (%s != %s)", key.Name, old.GetName())
		}

		// Can not change creation timestamps+user
		if obj.GetCreatedBy() != old.GetCreatedBy() {
			return event.BadRequest(err, "can not change the created by metadata (%s != %s)", obj.GetCreatedBy(), old.GetCreatedBy())
		}
		if obj.GetCreationTimestamp() != old.GetCreationTimestamp() {
			return event.BadRequest(err, "can not change the CreationTimestamp metadata (%v != %v)", obj.GetCreationTimestamp(), old.GetCreationTimestamp())
		}

		oldFolder := obj.GetFolder()
		if oldFolder != folder {
			event.FolderChanged = true
		}
		event.OldObject = old
	} else if folder != "" {
		event.FolderChanged = true
	}
	return event
}

func (v *eventValidator) PrepareCreate(ctx context.Context, req *CreateRequest) (*WriteEvent, error) {
	event := v.newEvent(ctx, req.Key, req.Value, nil)
	event.Operation = ResourceOperation_CREATED
	if event.Status != nil {
		return event, nil
	}

	// Make sure the created by user is accurate
	//----------------------------------------
	val := event.Object.GetCreatedBy()
	if val != "" && val != event.Requester.GetUID().String() {
		return event.BadRequest(nil, "created by annotation does not match: metadata.annotations#"+utils.AnnoKeyCreatedBy), nil
	}

	// Create can not have updated properties
	//----------------------------------------
	if event.Object.GetUpdatedBy() != "" {
		return event.BadRequest(nil, "unexpected metadata.annotations#"+utils.AnnoKeyCreatedBy), nil
	}
	ts, err := event.Object.GetUpdatedTimestamp()
	if err != nil {
		return event.BadRequest(nil, fmt.Sprintf("invalid timestamp: %s", err)), nil
	}
	if ts != nil {
		return event.BadRequest(nil, "unexpected metadata.annotations#"+utils.AnnoKeyUpdatedTimestamp), nil
	}
	return event, nil
}

func (v *eventValidator) PrepareUpdate(ctx context.Context, req *UpdateRequest, current []byte) (*WriteEvent, error) {
	event := v.newEvent(ctx, req.Key, req.Value, current)
	event.Operation = ResourceOperation_UPDATED
	if event.Status != nil {
		return event, nil
	}

	// Make sure the update user is accurate
	//----------------------------------------
	val := event.Object.GetUpdatedBy()
	if val != "" && val != event.Requester.GetUID().String() {
		return event.BadRequest(nil, "created by annotation does not match: metadata.annotations#"+utils.AnnoKeyUpdatedBy), nil
	}

	return event, nil
}
