// Package ent 存放 Ent 生成的代码。
//
// 重新生成：
//
//	go generate ./ent
//
// --feature sql/upsert：让 Create() 支持 OnConflict（房间的"插入或更新"就是靠它，
// 比"先查再写"少一次往返，并且并发下不会撞唯一键）。
//
// 生成的代码**不要手改**——改 ent/schema 下的 schema 定义再重新生成。
package ent

//go:generate go run -mod=mod entgo.io/ent/cmd/ent generate --feature sql/upsert ./schema
