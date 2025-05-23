package grpc

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/chroma-core/chroma/go/pkg/common"
	"github.com/chroma-core/chroma/go/pkg/grpcutils"
	"github.com/chroma-core/chroma/go/pkg/sysdb/coordinator/model"
	"github.com/chroma-core/chroma/go/pkg/sysdb/metastore/db/dao"
	"github.com/chroma-core/chroma/go/pkg/sysdb/metastore/db/dbcore"
	s3metastore "github.com/chroma-core/chroma/go/pkg/sysdb/metastore/s3"
	"github.com/chroma-core/chroma/go/pkg/types"
	"github.com/pingcap/log"
	"github.com/stretchr/testify/suite"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

type CleanupTestSuite struct {
	suite.Suite
	db           *gorm.DB
	read_db      *gorm.DB
	s            *Server
	tenantName   string
	databaseName string
	databaseId   string
}

func (suite *CleanupTestSuite) SetupSuite() {
	log.Info("setup suite")
	suite.db, suite.read_db = dbcore.ConfigDatabaseForTesting()
	s, err := NewWithGrpcProvider(Config{
		SystemCatalogProvider:      "database",
		SoftDeleteEnabled:          true,
		SoftDeleteCleanupInterval:  1 * time.Second,
		SoftDeleteMaxAge:           0,
		SoftDeleteCleanupBatchSize: 10,
		Testing:                    true,
		MetaStoreConfig: s3metastore.S3MetaStoreConfig{
			BucketName:              "test-bucket",
			Region:                  "us-east-1",
			Endpoint:                "http://localhost:9000",
			AccessKeyID:             "minio",
			SecretAccessKey:         "minio123",
			ForcePathStyle:          true,
			CreateBucketIfNotExists: true,
		},
	}, grpcutils.Default)
	if err != nil {
		suite.T().Fatalf("error creating server: %v", err)
	}
	suite.s = s
	suite.tenantName = "tenant_" + suite.T().Name()
	suite.databaseName = "database_" + suite.T().Name()
	DbId, err := dao.CreateTestTenantAndDatabase(suite.db, suite.tenantName, suite.databaseName)
	suite.NoError(err)
	suite.databaseId = DbId
}

func (suite *CleanupTestSuite) TearDownSuite() {
	log.Info("teardown suite")
	err := dao.CleanUpTestDatabase(suite.db, suite.tenantName, suite.databaseName)
	suite.NoError(err)
	err = dao.CleanUpTestTenant(suite.db, suite.tenantName)
	suite.NoError(err)
}

func (suite *CleanupTestSuite) TestSoftDeleteCleanup() {
	// Create 2 test collections
	collections := make([]string, 2)
	for i := 0; i < 2; i++ {
		collectionName := "cleanup_test_collection_" + strconv.Itoa(i)
		collectionID, err := dao.CreateTestCollection(suite.db, collectionName, 128, suite.databaseId, nil)
		suite.NoError(err)
		collections[i] = collectionID
	}

	// Soft delete both collections
	for _, collectionID := range collections {
		err := suite.s.coordinator.DeleteCollection(context.Background(), &model.DeleteCollection{
			ID: types.MustParse(collectionID),
		})
		suite.NoError(err)
	}

	// Verify collections are soft deleted
	softDeletedCollections, err := suite.s.coordinator.GetSoftDeletedCollections(context.Background(), nil, "", "", 10)
	suite.NoError(err)
	suite.Equal(2, len(softDeletedCollections))

	// Start the cleaner.
	suite.s.softDeleteCleaner.maxInitialJitter = 0 * time.Second
	suite.s.softDeleteCleaner.Start()

	// Wait for cleanup cycle
	time.Sleep(3 * time.Second)

	// Verify collections are permanently deleted
	softDeletedCollections, err = suite.s.coordinator.GetSoftDeletedCollections(context.Background(), nil, "", "", 10)
	suite.NoError(err)
	log.Info("softDeletedCollections", zap.Any("softDeletedCollections", softDeletedCollections))
	suite.Equal(0, len(softDeletedCollections))

	// Stop the cleaner.
	suite.s.softDeleteCleaner.Stop()

	// Create a test collection
	collectionName := "cleanup_test_collection_double_delete"
	collectionID, err := dao.CreateTestCollection(suite.db, collectionName, 128, suite.databaseId, nil)
	suite.NoError(err)

	// Hard delete it once
	err = suite.s.coordinator.DeleteCollection(context.Background(), &model.DeleteCollection{
		ID: types.MustParse(collectionID),
	})
	suite.NoError(err)

	// Call CleanupSoftDeletedCollection twice.
	// This is to account for the Cleanup loop deleting the collection twice from separate nodes.
	// It will return ErrCollectionDeleteNonExistingCollection after the first deletion.
	err = suite.s.coordinator.CleanupSoftDeletedCollection(context.Background(), &model.DeleteCollection{
		ID:           types.MustParse(collectionID),
		DatabaseName: suite.databaseName,
	})
	suite.NoError(err)

	err = suite.s.coordinator.CleanupSoftDeletedCollection(context.Background(), &model.DeleteCollection{
		ID:           types.MustParse(collectionID),
		DatabaseName: suite.databaseName,
	})
	// Check that it returns ErrCollectionDeleteNonExistingCollection after the first deletion.
	suite.ErrorIs(err, common.ErrCollectionDeleteNonExistingCollection)

}

func (suite *CleanupTestSuite) TestSoftDeleteCleanupForkedCollection() {
	// Create 3 test collections
	collections := make([]string, 3)
	print("Creating collection")
	collectionName := "cleanup_root_test_collection"
	lineageFileName := "lineageFileName"
	collectionID, err := dao.CreateTestCollection(suite.db, collectionName, 128, suite.databaseId, &lineageFileName)
	suite.NoError(err)
	collections[0] = collectionID

	for i := 1; i < 3; i++ {
		collectionName := "cleanup_non_root_test_collection_" + strconv.Itoa(i)
		collectionID, err := dao.CreateTestCollection(suite.db, collectionName, 128, suite.databaseId, nil)
		suite.NoError(err)
		collections[i] = collectionID
	}

	// Soft delete the collections
	for _, collectionID := range collections {
		err := suite.s.coordinator.DeleteCollection(context.Background(), &model.DeleteCollection{
			ID: types.MustParse(collectionID),
		})
		suite.NoError(err)
	}

	// Verify collections are soft deleted
	softDeletedCollections, err := suite.s.coordinator.GetSoftDeletedCollections(context.Background(), nil, "", "", 10)
	suite.NoError(err)
	suite.Equal(3, len(softDeletedCollections))

	// Start the cleaner.
	suite.s.softDeleteCleaner.maxInitialJitter = 0 * time.Second
	suite.s.softDeleteCleaner.Start()

	// Wait for cleanup cycle
	time.Sleep(3 * time.Second)

	// Verify root collection is not hard deleted.
	softDeletedCollections, err = suite.s.coordinator.GetSoftDeletedCollections(context.Background(), nil, "", "", 10)
	suite.NoError(err)
	log.Info("softDeletedCollections", zap.Any("softDeletedCollections", softDeletedCollections))
	suite.Equal(1, len(softDeletedCollections))
	suite.Equal(softDeletedCollections[0].ID, types.MustParse(collections[0]))

	// Stop the cleaner.
	suite.s.softDeleteCleaner.Stop()
}

func TestCleanupTestSuite(t *testing.T) {
	testSuite := new(CleanupTestSuite)
	suite.Run(t, testSuite)
}
