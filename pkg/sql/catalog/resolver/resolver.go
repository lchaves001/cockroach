// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package resolver

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catalogkv"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondata"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/errors"
)

// SchemaResolver abstracts the interfaces needed from the logical
// planner to perform name resolution below.
//
// We use an interface instead of passing *planner directly to make
// the resolution methods able to work even when we evolve the code to
// use a different plan builder.
// TODO(rytaft,andyk): study and reuse this.
type SchemaResolver interface {
	tree.ObjectNameExistingResolver
	tree.ObjectNameTargetResolver

	Txn() *kv.Txn
	LogicalSchemaAccessor() catalog.Accessor
	CurrentDatabase() string
	CurrentSearchPath() sessiondata.SearchPath
	CommonLookupFlags(required bool) tree.CommonLookupFlags
	ObjectLookupFlags(required bool, requireMutable bool) tree.ObjectLookupFlags
	LookupTableByID(ctx context.Context, id sqlbase.ID) (catalog.TableEntry, error)
}

// ErrNoPrimaryKey is returned when resolving a table object and the
// AllowWithoutPrimaryKey flag is not set.
var ErrNoPrimaryKey = pgerror.Newf(pgcode.NoPrimaryKey,
	"requested table does not have a primary key")

// GetObjectNames retrieves the names of all objects in the target database/
// schema. If explicitPrefix is set, the returned table names will have an
// explicit schema and catalog name.
func GetObjectNames(
	ctx context.Context,
	txn *kv.Txn,
	sc SchemaResolver,
	codec keys.SQLCodec,
	dbDesc sqlbase.DatabaseDescriptorInterface,
	scName string,
	explicitPrefix bool,
) (res tree.TableNames, err error) {
	return sc.LogicalSchemaAccessor().GetObjectNames(ctx, txn, codec, dbDesc, scName,
		tree.DatabaseListFlags{
			CommonLookupFlags: sc.CommonLookupFlags(true /* required */),
			ExplicitPrefix:    explicitPrefix,
		})
}

// ResolveExistingTableObject looks up an existing object.
// If required is true, an error is returned if the object does not exist.
// Optionally, if a desired descriptor type is specified, that type is checked.
//
// The object name is modified in-place with the result of the name
// resolution, if successful. It is not modified in case of error or
// if no object is found.
func ResolveExistingTableObject(
	ctx context.Context, sc SchemaResolver, tn *tree.TableName, lookupFlags tree.ObjectLookupFlags,
) (res *sqlbase.ImmutableTableDescriptor, err error) {
	// TODO: As part of work for #34240, an UnresolvedObjectName should be
	//  passed as an argument to this function.
	un := tn.ToUnresolvedObjectName()
	desc, prefix, err := ResolveExistingObject(ctx, sc, un, lookupFlags)
	if err != nil || desc == nil {
		return nil, err
	}
	tn.ObjectNamePrefix = prefix
	return desc.(*sqlbase.ImmutableTableDescriptor), nil
}

// ResolveMutableExistingTableObject looks up an existing mutable object.
// If required is true, an error is returned if the object does not exist.
// Optionally, if a desired descriptor type is specified, that type is checked.
//
// The object name is modified in-place with the result of the name
// resolution, if successful. It is not modified in case of error or
// if no object is found.
func ResolveMutableExistingTableObject(
	ctx context.Context,
	sc SchemaResolver,
	tn *tree.TableName,
	required bool,
	requiredType tree.RequiredTableKind,
) (res *sqlbase.MutableTableDescriptor, err error) {
	lookupFlags := tree.ObjectLookupFlags{
		CommonLookupFlags:    tree.CommonLookupFlags{Required: required},
		RequireMutable:       true,
		DesiredObjectKind:    tree.TableObject,
		DesiredTableDescKind: requiredType,
	}
	// TODO: As part of work for #34240, an UnresolvedObjectName should be
	// passed as an argument to this function.
	un := tn.ToUnresolvedObjectName()
	desc, prefix, err := ResolveExistingObject(ctx, sc, un, lookupFlags)
	if err != nil || desc == nil {
		return nil, err
	}
	tn.ObjectNamePrefix = prefix
	return desc.(*sqlbase.MutableTableDescriptor), nil
}

// ResolveMutableType resolves a type descriptor for mutable access. It
// returns the resolved descriptor, as well as the fully qualified resolved
// object name.
func ResolveMutableType(
	ctx context.Context, sc SchemaResolver, un *tree.UnresolvedObjectName, required bool,
) (*tree.TypeName, *sqlbase.MutableTypeDescriptor, error) {
	lookupFlags := tree.ObjectLookupFlags{
		CommonLookupFlags: tree.CommonLookupFlags{Required: required},
		RequireMutable:    true,
		DesiredObjectKind: tree.TypeObject,
	}
	desc, prefix, err := ResolveExistingObject(ctx, sc, un, lookupFlags)
	if err != nil || desc == nil {
		return nil, nil, err
	}
	tn := tree.MakeNewQualifiedTypeName(prefix.Catalog(), prefix.Schema(), un.Object())
	return &tn, desc.(*sqlbase.MutableTypeDescriptor), nil
}

// ResolveExistingObject resolves an object with the given flags.
func ResolveExistingObject(
	ctx context.Context,
	sc SchemaResolver,
	un *tree.UnresolvedObjectName,
	lookupFlags tree.ObjectLookupFlags,
) (res tree.NameResolutionResult, prefix tree.ObjectNamePrefix, err error) {
	found, prefix, descI, err := tree.ResolveExisting(ctx, un, sc, lookupFlags, sc.CurrentDatabase(), sc.CurrentSearchPath())
	if err != nil {
		return nil, prefix, err
	}
	// Construct the resolved table name for use in error messages.
	resolvedTn := tree.MakeTableNameFromPrefix(prefix, tree.Name(un.Object()))
	if !found {
		if lookupFlags.Required {
			return nil, prefix, sqlbase.NewUndefinedObjectError(&resolvedTn, lookupFlags.DesiredObjectKind)
		}
		return nil, prefix, nil
	}

	obj := descI.(catalog.Descriptor)
	switch lookupFlags.DesiredObjectKind {
	case tree.TypeObject:
		if obj.TypeDesc() == nil {
			return nil, prefix, sqlbase.NewUndefinedTypeError(&resolvedTn)
		}
		if lookupFlags.RequireMutable {
			return obj.(*sqlbase.MutableTypeDescriptor), prefix, nil
		}
		return obj.(*sqlbase.ImmutableTypeDescriptor), prefix, nil
	case tree.TableObject:
		if obj.TableDesc() == nil {
			return nil, prefix, sqlbase.NewUndefinedRelationError(&resolvedTn)
		}
		goodType := true
		switch lookupFlags.DesiredTableDescKind {
		case tree.ResolveRequireTableDesc:
			goodType = obj.TableDesc().IsTable()
		case tree.ResolveRequireViewDesc:
			goodType = obj.TableDesc().IsView()
		case tree.ResolveRequireTableOrViewDesc:
			goodType = obj.TableDesc().IsTable() || obj.TableDesc().IsView()
		case tree.ResolveRequireSequenceDesc:
			goodType = obj.TableDesc().IsSequence()
		}
		if !goodType {
			return nil, prefix, sqlbase.NewWrongObjectTypeError(&resolvedTn, lookupFlags.DesiredTableDescKind.String())
		}

		// If the table does not have a primary key, return an error
		// that the requested descriptor is invalid for use.
		if !lookupFlags.AllowWithoutPrimaryKey &&
			obj.TableDesc().IsTable() &&
			!obj.TableDesc().HasPrimaryKey() {
			return nil, prefix, ErrNoPrimaryKey
		}

		if lookupFlags.RequireMutable {
			return descI.(*sqlbase.MutableTableDescriptor), prefix, nil
		}

		return descI.(*sqlbase.ImmutableTableDescriptor), prefix, nil
	default:
		return nil, prefix, errors.AssertionFailedf(
			"unknown desired object kind %d", lookupFlags.DesiredObjectKind)
	}
}

// ResolveTargetObject determines a valid target path for an object
// that may not exist yet. It returns the descriptor for the database
// where the target object lives. It also returns the resolved name
// prefix for the input object.
func ResolveTargetObject(
	ctx context.Context, sc SchemaResolver, un *tree.UnresolvedObjectName,
) (*catalog.ResolvedObjectPrefix, tree.ObjectNamePrefix, error) {
	found, prefix, scMeta, err := tree.ResolveTarget(ctx, un, sc, sc.CurrentDatabase(), sc.CurrentSearchPath())
	if err != nil {
		return nil, prefix, err
	}
	if !found {
		if !un.HasExplicitSchema() && !un.HasExplicitCatalog() {
			return nil, prefix, pgerror.New(pgcode.InvalidName, "no database specified")
		}
		err = pgerror.Newf(pgcode.InvalidSchemaName,
			"cannot create %q because the target database or schema does not exist",
			tree.ErrString(un))
		err = errors.WithHint(err, "verify that the current database and search_path are valid and/or the target database exists")
		return nil, prefix, err
	}
	scInfo := scMeta.(*catalog.ResolvedObjectPrefix)
	if scInfo.Schema.Kind == sqlbase.SchemaVirtual {
		return nil, prefix, pgerror.Newf(pgcode.InsufficientPrivilege,
			"schema cannot be modified: %q", tree.ErrString(&prefix))
	}
	return scInfo, prefix, nil
}

var staticSchemaIDMap = map[sqlbase.ID]string{
	keys.PublicSchemaID:         tree.PublicSchema,
	sqlbase.PgCatalogID:         sessiondata.PgCatalogName,
	sqlbase.InformationSchemaID: sessiondata.InformationSchemaName,
	sqlbase.CrdbInternalID:      sessiondata.CRDBInternalSchemaName,
	sqlbase.PgExtensionSchemaID: sessiondata.PgExtensionSchemaName,
}

// ResolveSchemaNameByID resolves a schema's name based on db and schema id.
// TODO(sqlexec): this should return the descriptor instead if given an ID.
// Instead, we have to rely on a scan of the kv table.
// TODO(sqlexec): this should probably be cached.
// TODO(ajwerner,lucyzhang): this should take a SchemaResolver and use it.
func ResolveSchemaNameByID(
	ctx context.Context, txn *kv.Txn, codec keys.SQLCodec, dbID sqlbase.ID, schemaID sqlbase.ID,
) (string, error) {
	// Fast-path for public schema and virtual schemas, to avoid hot lookups.
	for id, schemaName := range staticSchemaIDMap {
		if id == schemaID {
			return schemaName, nil
		}
	}
	schemas, err := GetForDatabase(ctx, txn, codec, dbID)
	if err != nil {
		return "", err
	}
	if schema, ok := schemas[schemaID]; ok {
		return schema, nil
	}
	return "", errors.Newf("unable to resolve schema id %d for db %d", schemaID, dbID)
}

// ResolveTypeDescByID resolves a TypeDescriptor and fully qualified name
// from an ID.
// TODO (rohany): Once we start to cache type descriptors, this needs to
//  look into the set of leased copies.
// TODO (rohany): Once we lease types, this should be pushed down into the
//  leased object collection.
func ResolveTypeDescByID(
	ctx context.Context,
	txn *kv.Txn,
	codec keys.SQLCodec,
	id sqlbase.ID,
	lookupFlags tree.ObjectLookupFlags,
) (tree.TypeName, sqlbase.TypeDescriptorInterface, error) {
	desc, err := catalogkv.GetDescriptorByID(ctx, txn, codec, id)
	if err != nil {
		return tree.TypeName{}, nil, err
	}
	if desc == nil {
		if lookupFlags.Required {
			return tree.TypeName{}, nil, pgerror.Newf(
				pgcode.UndefinedObject, "type with ID %d does not exist", id)
		}
		return tree.TypeName{}, nil, nil
	}
	if desc.TypeDesc() == nil {
		return tree.TypeName{}, nil, errors.AssertionFailedf("%s was not a type descriptor", desc)
	}
	// Get the parent database and schema names to create a fully qualified
	// name for the type.
	// TODO (SQLSchema): As we add leasing for all descriptors, these calls
	//  should look into those leased copies, rather than do raw reads.
	typDesc := desc.(*sqlbase.ImmutableTypeDescriptor)
	db, err := sqlbase.GetDatabaseDescFromID(ctx, txn, codec, typDesc.ParentID)
	if err != nil {
		return tree.TypeName{}, nil, err
	}
	schemaName, err := ResolveSchemaNameByID(ctx, txn, codec, typDesc.ParentID, typDesc.ParentSchemaID)
	if err != nil {
		return tree.TypeName{}, nil, err
	}
	name := tree.MakeNewQualifiedTypeName(db.GetName(), schemaName, typDesc.GetName())
	var ret sqlbase.TypeDescriptorInterface
	if lookupFlags.RequireMutable {
		// TODO(ajwerner): Figure this out later when we construct this inside of
		// the name resolution. This really shouldn't be happening here. Instead we
		// should be taking a SchemaResolver and resolving through it which should
		// be able to hit a descs.Collection and determine whether this is a new
		// type or not.
		desc = sqlbase.NewMutableExistingTypeDescriptor(*typDesc.TypeDesc())
	} else {
		ret = typDesc
	}
	return name, ret, nil
}

// GetForDatabase looks up and returns all available
// schema ids to names for a given database.
func GetForDatabase(
	ctx context.Context, txn *kv.Txn, codec keys.SQLCodec, dbID sqlbase.ID,
) (map[sqlbase.ID]string, error) {
	log.Eventf(ctx, "fetching all schema descriptor IDs for %d", dbID)

	nameKey := sqlbase.NewSchemaKey(dbID, "" /* name */).Key(codec)
	kvs, err := txn.Scan(ctx, nameKey, nameKey.PrefixEnd(), 0 /* maxRows */)
	if err != nil {
		return nil, err
	}

	// Always add public schema ID.
	// TODO(solon): This can be removed in 20.2, when this is always written.
	// In 20.1, in a migrating state, it may be not included yet.
	ret := make(map[sqlbase.ID]string, len(kvs)+1)
	ret[sqlbase.ID(keys.PublicSchemaID)] = tree.PublicSchema

	for _, kv := range kvs {
		id := sqlbase.ID(kv.ValueInt())
		if _, ok := ret[id]; ok {
			continue
		}
		_, _, name, err := sqlbase.DecodeNameMetadataKey(codec, kv.Key)
		if err != nil {
			return nil, err
		}
		ret[id] = name
	}
	return ret, nil
}
