package snet_syncer

import (
	"context"
	"encoding/json"
	"github.com/bufbuild/protocompile"
	"github.com/rs/zerolog/log"
	"google.golang.org/protobuf/reflect/protoreflect"
	"matrix-ai-framework/pkg/blockchain"
	"matrix-ai-framework/pkg/db"
	ipfs "matrix-ai-framework/pkg/ipfs"
	"strings"
	"time"
)

type SnetSyncer struct {
	Ethereum        blockchain.Ethereum
	IPFSClient      ipfs.IPFSClient
	DB              db.Service
	FileDescriptors map[string][]protoreflect.FileDescriptor
}

func New(eth blockchain.Ethereum, ipfs ipfs.IPFSClient, db db.Service) SnetSyncer {
	return SnetSyncer{
		Ethereum:        eth,
		IPFSClient:      ipfs,
		DB:              db,
		FileDescriptors: make(map[string][]protoreflect.FileDescriptor),
	}
}

func (s *SnetSyncer) syncOnce() {
	log.Info().Msg("SnetSyncer now working...")

	orgs, _ := s.Ethereum.GetOrgs()
	for _, orgIDBytes := range orgs {
		borg, err := s.Ethereum.GetOrg(orgIDBytes)
		if err != nil {
			log.Error().Err(err).Msg("Failed to get org")
			continue
		}
		var org blockchain.OrganizationMetaData

		metadataJson, err := s.IPFSClient.GetIpfsFile(string(borg.OrgMetadataURI))
		if err != nil {
			log.Error().Err(err).Msg("Failed to get ipfs file")
			continue
		}

		err = json.Unmarshal(metadataJson, &org)
		if err != nil {
			log.Error().Err(err).Any("content", string(metadataJson)).Msg("Can't unmarshal org metadata from ipfs")
			continue
		}

		org.Owner = borg.Owner.Hex()
		org.SnetID = strings.ReplaceAll(string(borg.Id[:]), "\u0000", "")
		dbOrg, dbGroups := org.DB()
		orgID, err := s.DB.CreateSnetOrg(dbOrg)
		if err != nil {
			log.Error().Err(err).Msg("Failed to create org")
		}
		org.ID = orgID
		err = s.DB.CreateSnetOrgGroups(orgID, dbGroups)
		if err != nil {
			log.Error().Err(err).Msg("Failed to create org group")
		}

		for _, serviceIDBytes := range borg.ServiceIds {
			service, err := s.Ethereum.GetService(borg.Id, serviceIDBytes)
			if err != nil {
				log.Error().Err(err)
				continue
			}

			metadataJson, err = s.IPFSClient.GetIpfsFile(string(service.MetadataURI))
			if err != nil {
				log.Error().Err(err).Msg("Failed to get file from ipfs")
				return
			}

			var srvMeta blockchain.ServiceMetadata
			err = json.Unmarshal(metadataJson, &srvMeta)
			if err != nil {
				log.Error().Err(err).Any("content", string(metadataJson)).Msg("Failed to unmarshal metadata from ipfs")
				return
			}

			log.Debug().Msgf("Metadata of service: %+v", srvMeta)

			srvMeta.OrgID = orgID
			srvMeta.SnetID = strings.ReplaceAll(string(serviceIDBytes[:]), "\u0000", "")
			srvMeta.SnetOrgID = org.SnetID
			srvMeta.ID, err = s.DB.CreateSnetService(srvMeta.DB())
			if err != nil {
				log.Error().Err(err).Int("id", srvMeta.ID).Str("snet-id", srvMeta.SnetID).Msg("Failed to add snet_service")
			}

			content, err := s.IPFSClient.GetIpfsFile(srvMeta.ModelIpfsHash)
			if err != nil {
				log.Error().Err(err)
			}
			protoFiles, err := ipfs.ReadFilesCompressed(string(content))
			if err != nil {
				log.Error().Err(err)
			}

			for fileName, fileContent := range protoFiles {
				fd := getFileDescriptor(string(fileContent), fileName)
				s.FileDescriptors[srvMeta.SnetID] = append(s.FileDescriptors[srvMeta.SnetID], fd)
			}
		}
	}
}

func (s *SnetSyncer) Start() {
	log.Info().Msg("SnetSyncer started")
	s.syncOnce()
	ticker := time.NewTicker(100 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.syncOnce()
		}
	}
}

func getFileDescriptor(protoContent, name string) (ds protoreflect.FileDescriptor) {
	accessor := protocompile.SourceAccessorFromMap(map[string]string{
		name: protoContent,
	})
	compiler := protocompile.Compiler{
		Resolver:       &protocompile.SourceResolver{Accessor: accessor},
		SourceInfoMode: protocompile.SourceInfoStandard,
	}
	fds, err := compiler.Compile(context.Background(), name)
	if err != nil {
		log.Error().Err(err).Msg("Failed to create file descriptor")
		return
	}
	ds = fds.FindFileByPath(name)
	return
}

func (s *SnetSyncer) GetSnetServicesInfo() string {
	var builder strings.Builder
	if s.FileDescriptors != nil {
		builder.WriteString("<div style=\"line-height: 0.8;\"><ol>")
		for snetID, descriptors := range s.FileDescriptors {
			if descriptors != nil {
				for _, descriptor := range descriptors {
					if descriptor != nil {
						builder.WriteString("<li><strong>Path: " + descriptor.Path() + " Snet ID: " + snetID + " Descriptor: " + string(descriptor.FullName().Name()) + "</strong></li>")
						services := descriptor.Services()
						if services != nil {
							for i := 0; i < services.Len(); i++ {
								if services.Get(i) != nil {
									builder.WriteString("<p><em>Service: " + string(services.Get(i).FullName().Name()) + "</em></p>")
									methods := services.Get(i).Methods()
									if methods != nil {
										builder.WriteString("<p>🔁Methods: </p><ul>")
										for j := 0; j < methods.Len(); j++ {
											if methods.Get(j) != nil {
												builder.WriteString("<li>" + string(methods.Get(j).FullName().Name()) + "<br>")
												inputFields := methods.Get(j).Input().Fields()
												outputFields := methods.Get(j).Output().Fields()

												if inputFields != nil {
													builder.WriteString("<p>➡️Input:</p>")
													builder.WriteString("<pre><code>{")
													for n := 0; n < inputFields.Len(); n++ {
														if inputFields.Get(n).Message() != nil {
															messageFields := inputFields.Get(n).Message().Fields()
															if messageFields != nil {
																builder.WriteString("\n    \"" + inputFields.Get(n).JSONName() + "\": {")
																for m := 0; m < messageFields.Len(); m++ {
																	builder.WriteString("\n        \"" + messageFields.Get(m).JSONName() + "\": " + messageFields.Get(m).Kind().String())
																}
																builder.WriteString("\n    }")
															}
														} else {
															builder.WriteString("\n    \"" + inputFields.Get(n).JSONName() + "\": " + inputFields.Get(n).Kind().String())
														}
													}
													builder.WriteString("\n}</code></pre>")
												}
												if outputFields != nil {
													builder.WriteString("<p>➡️Output:</p>")
													builder.WriteString("<pre><code>{")
													for n := 0; n < outputFields.Len(); n++ {
														if outputFields.Get(n).Message() != nil {
															messageFields := outputFields.Get(n).Message().Fields()
															if messageFields != nil {
																builder.WriteString("\n    \"" + outputFields.Get(n).JSONName() + "\": {")
																for m := 0; m < messageFields.Len(); m++ {
																	builder.WriteString("\n        \"" + messageFields.Get(m).JSONName() + "\": " + messageFields.Get(m).Kind().String())
																}
																builder.WriteString("\n    }")
															}
														} else {
															builder.WriteString("\n    \"" + outputFields.Get(n).JSONName() + "\": " + outputFields.Get(n).Kind().String())
														}
													}
													builder.WriteString("\n}</code></pre>")
												}
												builder.WriteString("</li>")
											}
										}
										builder.WriteString("</ul>")
									}
								}
							}
						}
					}
				}
			}
		}
		builder.WriteString("</ol></div>")

	}

	return builder.String()
}
