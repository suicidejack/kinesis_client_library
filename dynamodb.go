package kcl

import (
	"fmt"
	"strconv"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/awslabs/aws-sdk-go/aws"
	"github.com/awslabs/aws-sdk-go/aws/awserr"
	"github.com/awslabs/aws-sdk-go/aws/awsutil"
	"github.com/awslabs/aws-sdk-go/service/dynamodb"
)

type shardRecord struct {
	ShardID         string
	Checkpoint      string
	LeaseExpiration int64
	WorkerID        string
}

func (s shardRecord) String() string {
	return fmt.Sprintf("ShardID: %s, Checkpoint: %s, LeaseExpiration: %d, WorkerID: %s",
		s.ShardID,
		s.Checkpoint,
		s.LeaseExpiration,
		s.WorkerID,
	)
}

type dynamo struct {
	db            *dynamodb.DynamoDB
	tableName     string
	readCapacity  int64
	writeCapacity int64
}

func newDynamo(name string, readCapacity, writeCapacity int64) *dynamo {
	cfg := aws.DefaultConfig
	return &dynamo{
		db:            dynamodb.New(cfg),
		tableName:     name,
		readCapacity:  readCapacity,
		writeCapacity: writeCapacity,
	}
}

func (d *dynamo) ValidateTable() (err error) {
	err = d.findTable()
	if awserr, ok := err.(awserr.Error); ok {
		log.WithField("error", awserr).Error("awserror: unable to describe table")
		if awserr.Code() == "ResourceNotFoundException" {
			log.Error("we should create the table here")
			err = d.createTable()
		}
	} else {
	}
	return
}

func (d *dynamo) findTable() error {
	input := &dynamodb.DescribeTableInput{
		TableName: aws.String(d.tableName),
	}
	output, err := d.db.DescribeTable(input)
	if err != nil {
		return err
	}
	if !isValidTableSchema(output) {
		return fmt.Errorf("dynamo: invalid table schema")
	}
	return nil
}

func (d *dynamo) createTable() (err error) {
	tableDefinition := &dynamodb.CreateTableInput{
		TableName:            aws.String(d.tableName),
		AttributeDefinitions: make([]*dynamodb.AttributeDefinition, 1, 1),
		KeySchema:            make([]*dynamodb.KeySchemaElement, 1, 1),
		ProvisionedThroughput: &dynamodb.ProvisionedThroughput{
			ReadCapacityUnits:  aws.Long(d.readCapacity),
			WriteCapacityUnits: aws.Long(d.writeCapacity),
		},
	}
	tableDefinition.KeySchema[0] = &dynamodb.KeySchemaElement{
		AttributeName: aws.String("shard_id"),
		KeyType:       aws.String("HASH"),
	}
	tableDefinition.AttributeDefinitions[0] = &dynamodb.AttributeDefinition{
		AttributeName: aws.String("shard_id"),
		AttributeType: aws.String("S"),
	}
	var out *dynamodb.CreateTableOutput
	out, err = d.db.CreateTable(tableDefinition)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
			"out":   out,
		}).Error("unable to create table")
		return
	}
	if out != nil && out.TableDescription != nil {
		log.WithFields(log.Fields{
			"TableStatus": stringPtrToString(out.TableDescription.TableStatus),
			"TableName":   d.tableName,
		}).Debug("created dynamodb table")
	}

	d.validateTableCreated()

	return
}

// blocks until the table status comes back as "ACTIVE"
func (d *dynamo) validateTableCreated() {
	input := &dynamodb.DescribeTableInput{
		TableName: aws.String(d.tableName),
	}
	isActive := false

	for !isActive {
		time.Sleep(1 * time.Second)
		if out, err := d.db.DescribeTable(input); err == nil {
			log.WithField("status", awsutil.StringValue(out.Table.TableStatus)).Debug("got describe table output")
			if stringPtrToString(out.Table.TableStatus) == "ACTIVE" {
				isActive = true
			}
		}
	}
}

func (d *dynamo) Checkpoint(shardID, seqNum, leaseExpiration, workerID string) (err error) {
	attributes := map[string]*dynamodb.AttributeValue{
		"shard_id": &dynamodb.AttributeValue{
			S: aws.String(shardID),
		},
		"checkpoint": &dynamodb.AttributeValue{
			S: aws.String(seqNum),
		},
		"lease_expiration": &dynamodb.AttributeValue{
			N: aws.String(leaseExpiration),
		},
		"worker_id": &dynamodb.AttributeValue{
			S: aws.String(workerID),
		},
	}
	input := &dynamodb.PutItemInput{
		TableName: aws.String(d.tableName),
		Item:      &attributes,
	}
	_, err = d.db.PutItem(input)

	return
}

// TODO: handling unprocessed records - working? need more shards to test
func (d *dynamo) GetShardData(shards []string) (shardRecords map[string]*shardRecord, err error) {
	funcName := "GetShardData"
	shardRecords = make(map[string]*shardRecord)

	// form the request for the records
	keys := make([]*map[string]*dynamodb.AttributeValue, len(shards), len(shards))
	for i, shard := range shards {
		keys[i] = &map[string]*dynamodb.AttributeValue{
			"shard_id": &dynamodb.AttributeValue{
				S: aws.String(shard),
			},
		}
	}

	keysToProcess := &map[string]*dynamodb.KeysAndAttributes{
		d.tableName: &dynamodb.KeysAndAttributes{
			Keys:                 keys,
			ProjectionExpression: aws.String("shard_id,checkpoint,lease_expiration,worker_id"),
			ConsistentRead:       aws.Boolean(true),
		},
	}

	for keysToProcess != nil && len(*keysToProcess) > 0 {
		input := &dynamodb.BatchGetItemInput{
			RequestItems: keysToProcess,
		}
		var out *dynamodb.BatchGetItemOutput
		out, err = d.db.BatchGetItem(input)
		if err != nil {
			log.WithFields(log.Fields{
				"error":    err,
				"function": funcName,
			}).Error("unable to batch get items")
			return
		}
		records := d.parseShardData(out)
		for key, record := range records {
			shardRecords[key] = record
		}
		keysToProcess = out.UnprocessedKeys
		log.WithFields(log.Fields{
			"function":      funcName,
			"keysToProcess": keysToProcess,
			"length":        len(*keysToProcess),
		}).Debug("dynamo iteration")
	}

	return
}

func (d *dynamo) parseShardData(resp *dynamodb.BatchGetItemOutput) (shardRecords map[string]*shardRecord) {
	funcName := "ParseShardData"
	if resp == nil {
		log.WithField("function", funcName).Error("resp is nil")
		return
	}
	if resp.Responses == nil {
		log.WithField("function", funcName).Error("resp.Responses is nil")
		return
	}
	var records []*map[string]*dynamodb.AttributeValue
	var ok bool
	if records, ok = (*resp.Responses)[d.tableName]; !ok {
		log.WithField("function", funcName).Error("could not find table")
		return
	}

	if len(records) == 0 {
		log.WithFields(log.Fields{
			"function": funcName,
		}).Debug("there are no records in dynamodb")
	}

	shardRecords = make(map[string]*shardRecord)
	for _, record := range records {
		shardID := stringPtrToString((*record)["shard_id"].S)
		leaseExpiration, _ := strconv.ParseInt(stringPtrToString((*record)["lease_expiration"].N), 10, 64)
		shardRecords[shardID] = &shardRecord{
			ShardID:         shardID,
			Checkpoint:      stringPtrToString((*record)["checkpoint"].S),
			LeaseExpiration: leaseExpiration,
			WorkerID:        stringPtrToString((*record)["worker_id"].S),
		}
	}

	return
}
