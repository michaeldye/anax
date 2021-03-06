package agreementbot

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/boltdb/bolt"
	"github.com/golang/glog"
	"github.com/open-horizon/anax/abstractprotocol"
	"github.com/open-horizon/anax/config"
	"github.com/open-horizon/anax/cutil"
	"github.com/open-horizon/anax/exchange"
	"github.com/open-horizon/anax/policy"
	"math/rand"
	"net/http"
)

// These structs are the event bodies that flow from the processor to the agreement workers
const INITIATE = "INITIATE_AGREEMENT"
const REPLY = "AGREEMENT_REPLY"
const CANCEL = "AGREEMENT_CANCEL"
const DATARECEIVEDACK = "AGREEMENT_DATARECEIVED_ACK"
const WORKLOAD_UPGRADE = "WORKLOAD_UPGRADE"
const ASYNC_CANCEL = "ASYNC_CANCEL"

type AgreementWork interface {
	Type() string
}

type InitiateAgreement struct {
	workType               string
	ProducerPolicy         policy.Policy               // the producer policy received from the exchange - demarshalled
	OriginalProducerPolicy string                      // the producer policy received from the exchange - original in string form to be sent back
	ConsumerPolicy         policy.Policy               // the consumer policy we're matched up with - this is a copy so that we can modify/augment it
	Org                    string                      // the org from which the consumer policy originated
	Device                 exchange.SearchResultDevice // the device entry in the exchange
}

func (c InitiateAgreement) String() string {
	res := ""
	res += fmt.Sprintf("Workitem: %v,  Org: %v\n", c.workType, c.Org)
	res += fmt.Sprintf("Producer Policy: %v\n", c.ProducerPolicy)
	res += fmt.Sprintf("Consumer Policy: %v\n", c.ConsumerPolicy)
	res += fmt.Sprintf("Device: %v", c.Device)
	return res
}

func (c InitiateAgreement) Type() string {
	return c.workType
}

type HandleReply struct {
	workType     string
	Reply        abstractprotocol.ProposalReply
	From         string // deprecated whisper address
	SenderId     string // exchange Id of sender
	SenderPubKey []byte
	MessageId    int
}

func (c HandleReply) String() string {
	return fmt.Sprintf("Workitem: %v, SenderId: %v, MessageId: %v, From: %v, Reply: %v, SenderPubKey: %x", c.workType, c.SenderId, c.MessageId, c.From, c.Reply, c.SenderPubKey)
}

func (c HandleReply) Type() string {
	return c.workType
}

type HandleDataReceivedAck struct {
	workType     string
	Ack          string
	From         string // deprecated whisper address
	SenderId     string // exchange Id of sender
	SenderPubKey []byte
	MessageId    int
}

func (c HandleDataReceivedAck) String() string {
	return fmt.Sprintf("Workitem: %v, SenderId: %v, MessageId: %v, From: %v, Ack: %v, SenderPubKey: %x", c.workType, c.SenderId, c.MessageId, c.From, c.Ack, c.SenderPubKey)
}

func (c HandleDataReceivedAck) Type() string {
	return c.workType
}

type CancelAgreement struct {
	workType    string
	AgreementId string
	Protocol    string
	Reason      uint
}

func (c CancelAgreement) Type() string {
	return c.workType
}

type HandleWorkloadUpgrade struct {
	workType    string
	AgreementId string
	Protocol    string
	Device      string
	PolicyName  string
}

func (c HandleWorkloadUpgrade) Type() string {
	return c.workType
}

type AsyncCancelAgreement struct {
	workType    string
	AgreementId string
	Protocol    string
	Reason      uint
}

func (c AsyncCancelAgreement) Type() string {
	return c.workType
}

type AgreementWorker interface {
	AgreementLockManager() *AgreementLockManager
}

type BaseAgreementWorker struct {
	pm         *policy.PolicyManager
	db         *bolt.DB
	config     *config.HorizonConfig
	alm        *AgreementLockManager
	workerID   string
	httpClient *http.Client
}

func (b *BaseAgreementWorker) AgreementLockManager() *AgreementLockManager {
	return b.alm
}

func (b *BaseAgreementWorker) InitiateNewAgreement(cph ConsumerProtocolHandler, wi *InitiateAgreement, random *rand.Rand, workerId string) {

	// Generate an agreement ID
	agreementIdString, aerr := cutil.GenerateAgreementId()
	if aerr != nil {
		glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error generating agreement id %v", aerr)))
		return
	}
	glog.V(5).Infof(BAWlogstring(workerId, fmt.Sprintf("using AgreementId %v", agreementIdString)))

	bcType, bcName, bcOrg := (&wi.ProducerPolicy).RequiresKnownBC(cph.Name())

	// Use the blockchain name to choose the handler
	protocolHandler := cph.AgreementProtocolHandler(bcType, bcName, bcOrg)
	if protocolHandler == nil {
		glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("agreement protocol handler is not ready yet for %v %v", bcType, bcName)))
		return
	}

	// Get the agreement id lock to prevent any other thread from processing this same agreement.
	lock := b.alm.getAgreementLock(agreementIdString)
	lock.Lock()
	defer lock.Unlock()

	// The device object we're working with might not include the policies for the microservices needed by the
	// workload in the curent consumer policy. If that's the case, query the exchange to get all the device
	// policies so we can merge them.
	var exchangeDev *exchange.Device
	if wi.ConsumerPolicy.PatternId != "" {
		if theDev, err := GetDevice(b.config.Collaborators.HTTPClientFactory.NewHTTPClient(nil), wi.Device.Id, b.config.AgreementBot.ExchangeURL, cph.ExchangeId(), cph.ExchangeToken()); err != nil {
			glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error getting device %v policies, error: %v", wi.Device.Id, err)))
			return
		} else {
			exchangeDev = theDev
		}
	}

	// There could be more than 1 workload version in the consumer policy, and each version might NOT require the exact same
	// microservices (and versions), so we first need to choose a workload. Choosing a workload is based on the priority of
	// each workload and whether or not this workload has been tried before. Also, iterate the loop more than once if we choose
	// a workload entry that turns out to be unsupportable by the device.
	foundWorkload := false
	var workload, lastWorkload *policy.Workload

	for !foundWorkload {

		if wlUsage, err := FindSingleWorkloadUsageByDeviceAndPolicyName(b.db, wi.Device.Id, wi.ConsumerPolicy.Header.Name); err != nil {
			glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error searching for persistent workload usage records for device %v with policy %v, error: %v", wi.Device.Id, wi.ConsumerPolicy.Header.Name, err)))
			return
		} else if wlUsage == nil {
			workload = wi.ConsumerPolicy.NextHighestPriorityWorkload(0, 0, 0)
		} else if wlUsage.DisableRetry {
			workload = wi.ConsumerPolicy.NextHighestPriorityWorkload(wlUsage.Priority, 0, wlUsage.FirstTryTime)
		} else if wlUsage != nil {
			workload = wi.ConsumerPolicy.NextHighestPriorityWorkload(wlUsage.Priority, wlUsage.RetryCount+1, wlUsage.FirstTryTime)
		}

		// If we chose the same workload 2 times in a row through this loop, then we need to exit out of here
		if lastWorkload == workload {
			glog.Warningf(BAWlogstring(workerId, fmt.Sprintf("unable to find supported workload for %v within %v", wi.Device.Id, wi.ConsumerPolicy.Workloads)))

			// If we created a workload usage record during this process, get rid of it.
			if err := DeleteWorkloadUsage(b.db, wi.Device.Id, wi.ConsumerPolicy.Header.Name); err != nil {
				glog.Warningf(BAWlogstring(workerId, fmt.Sprintf("unable to delete workload usage record for %v with %v because %v", wi.Device.Id, wi.ConsumerPolicy.Header.Name, err)))
			}
			return
		}

		// The workload in the consumer policy has a reference to the workload details. We need to get the details so that we
		// can verify that the device has the right version API specs to run this workload. Then, we can store the workload details
		// into the consumer policy file. We have a copy of the consumer policy file that we can modify. If the device doesnt have the right
		// version API specs, then we will try the next workload.

		if workloadDetails, err := exchange.GetWorkload(b.config.Collaborators.HTTPClientFactory, workload.WorkloadURL, workload.Org, workload.Version, workload.Arch, b.config.AgreementBot.ExchangeURL, cph.ExchangeId(), cph.ExchangeToken()); err != nil {
			glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error searching for workload details %v, error: %v", workload, err)))
			return
		} else if workloadDetails == nil {
			glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("unable to find workload %v on the exchange.", workload)))
			return
		} else {

			// Convert the workload details APISpec list to policy types, and merge the device side microservice policies if necessary.
			var mergedProducer *policy.Policy
			asl := new(policy.APISpecList)
			for _, apiSpec := range workloadDetails.APISpecs {
				(*asl) = append((*asl), (*policy.APISpecification_Factory(apiSpec.SpecRef, apiSpec.Org, apiSpec.Version, apiSpec.Arch)))
				if wi.ConsumerPolicy.PatternId != "" {
					for _, devMS := range exchangeDev.RegisteredMicroservices {
						// Find the device's microservice definition based on the microservices needed by the workload.
						if devMS.Url == apiSpec.SpecRef {
							if pol, err := policy.DemarshalPolicy(devMS.Policy); err != nil {
								glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error demarshalling device %v policy, error: %v", wi.Device.Id, err)))
								return
							} else if mergedProducer == nil {
								mergedProducer = pol
							} else if newPolicy, err := policy.Are_Compatible_Producers(mergedProducer, pol, b.config.AgreementBot.NoDataIntervalS); err != nil {
								glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error merging policies %v and %v, error: %v", mergedProducer, pol, err)))
								return
							} else {
								mergedProducer = newPolicy
							}
							break
						}
					}
				}
			}

			// Update the producer policy with a real merged policy based on the microservices required by the workload
			if wi.ConsumerPolicy.PatternId != "" && mergedProducer != nil {
				wi.ProducerPolicy = *mergedProducer
			}

			// If the device doesnt support the workload requirements, then remember that we rejected a higher priority workload because of
			// device requirements not being met. This will cause agreement cancellation to try the highest priority workload again
			// even if retries have been disabled.
			if err := wi.ProducerPolicy.APISpecs.Supports(*asl); err != nil {
				glog.Warningf(BAWlogstring(workerId, fmt.Sprintf("skipping workload %v because device %v cant support it: %v", workload, wi.Device.Id, err)))

				if !workload.HasEmptyPriority() {
					// If this is not the first time through the loop, update the workload usage record, otherwise create it.
					if lastWorkload != nil {
						if _, err := UpdatePriority(b.db, wi.Device.Id, wi.ConsumerPolicy.Header.Name, workload.Priority.PriorityValue, workload.Priority.RetryDurationS, workload.Priority.VerifiedDurationS, agreementIdString); err != nil {
							glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error updating priority in persistent workload usage records for device %v with policy %v, error: %v", wi.Device.Id, wi.ConsumerPolicy.Header.Name, err)))
							return
						}
					} else if err := NewWorkloadUsage(b.db, wi.Device.Id, wi.ProducerPolicy.HAGroup.Partners, "", wi.ConsumerPolicy.Header.Name, workload.Priority.PriorityValue, workload.Priority.RetryDurationS, workload.Priority.VerifiedDurationS, true, agreementIdString); err != nil {
						glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error creating persistent workload usage records for device %v with policy %v, error: %v", wi.Device.Id, wi.ConsumerPolicy.Header.Name, err)))
						return
					}

					// Artificially bump up the retry count so that the loop will choose the next workload
					if _, err := UpdateRetryCount(b.db, wi.Device.Id, wi.ConsumerPolicy.Header.Name, workload.Priority.Retries+1, agreementIdString); err != nil {
						glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error updating retry count persistent workload usage records for device %v with policy %v, error: %v", wi.Device.Id, wi.ConsumerPolicy.Header.Name, err)))
						return
					}
				}
			} else {

				foundWorkload = true
				// The device seems to support the required API specs, so augment the consumer policy file with the workload
				// details that match what the producer can support.
				wi.ConsumerPolicy.APISpecs = (*asl)

				// The agbot rejects workload definitions that dont have exactly 1 workload element in the workloads array so it is
				// safe to directly access the first element.
				workload.Deployment = workloadDetails.Workloads[0].Deployment
				workload.DeploymentSignature = workloadDetails.Workloads[0].DeploymentSignature
				torr := new(policy.Torrent)
				if workloadDetails.Workloads[0].Torrent != "" {
					if err := json.Unmarshal([]byte(workloadDetails.Workloads[0].Torrent), torr); err != nil {
						glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("Unable to demarshal torrent info from %v, error: %v", workloadDetails, err)))
						return
					}
				}
				workload.Torrent = *torr

				glog.V(5).Infof(BAWlogstring(workerId, fmt.Sprintf("workload %v is supported by device %v", workload, wi.Device.Id)))
			}

		}

		lastWorkload = workload
	}

	// Call the exchange to make sure that all partners are registered in the exchange. We can do this check now that we know
	// exactly what the merged producer policy looks like.
	if err := b.incompleteHAGroup(cph, &wi.ProducerPolicy); err != nil {
		glog.Warningf(BAWlogstring(workerId, fmt.Sprintf("received error checking HA group %v completeness for device %v, error: %v", wi.ProducerPolicy.HAGroup, wi.Device.Id, err)))
		return
	}

	// If this device is advertising a property that we are supposed to ignore, then skip it.
	if ignore, err := b.ignoreDevice(&wi.ProducerPolicy); err != nil {
		glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("received error checking for ignored device %v, error: %v", wi.Device.Id, err)))
		return
	} else if ignore {
		glog.V(3).Infof(BAWlogstring(workerId, fmt.Sprintf("skipping device %v, advertises ignored property", wi.Device.Id)))
		return
	}

	// Create pending agreement in database
	if err := AgreementAttempt(b.db, agreementIdString, wi.Org, wi.Device.Id, wi.ConsumerPolicy.Header.Name, bcType, bcName, bcOrg, cph.Name(), wi.ConsumerPolicy.PatternId, wi.ConsumerPolicy.NodeH); err != nil {
		glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error persisting agreement attempt: %v", err)))

		// Create message target for protocol message
	} else if mt, err := exchange.CreateMessageTarget(wi.Device.Id, nil, wi.Device.PublicKey, wi.Device.MsgEndPoint); err != nil {
		glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error creating message target: %v", err)))

		// Initiate the protocol
	} else if proposal, err := protocolHandler.InitiateAgreement(agreementIdString, &wi.ProducerPolicy, &wi.ConsumerPolicy, wi.Org, cph.ExchangeId(), mt, workload, b.config.AgreementBot.DefaultWorkloadPW, b.config.AgreementBot.NoDataIntervalS, cph.GetSendMessage()); err != nil {
		glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error initiating agreement: %v", err)))

		// Remove pending agreement from database
		if err := DeleteAgreement(b.db, agreementIdString, cph.Name()); err != nil {
			glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error deleting pending agreement: %v, error %v", agreementIdString, err)))
		}

		// TODO: Publish error on the message bus

		// Update the agreement in the DB with the proposal and policy
	} else if err := cph.PersistAgreement(wi, proposal, workerId); err != nil {
		glog.Errorf(err.Error())
	}

}

func (b *BaseAgreementWorker) HandleAgreementReply(cph ConsumerProtocolHandler, wi *HandleReply, workerId string) bool {

	reply := wi.Reply
	protocolHandler := cph.AgreementProtocolHandler("", "", "") // Use the generic protocol handler

	// The reply message is usually deleted before recording on the blockchain. For now assume it will be deleted at the end. Early exit from
	// this function is NOT allowed.
	deletedMessage := false

	// Get the agreement id lock to prevent any other thread from processing this same agreement.
	lock := b.alm.getAgreementLock(wi.Reply.AgreementId())
	lock.Lock()

	// The lock is dropped at the end of this function or right before the blockchain write. Early exit from this function is NOT allowed.
	droppedLock := false

	// Assume we will ack negatively unless we find out that everything is ok.
	ackReplyAsValid := false
	sendReply := true

	if reply.ProposalAccepted() {

		// Find the saved agreement in the database
		if agreement, err := FindSingleAgreementByAgreementId(b.db, reply.AgreementId(), cph.Name(), []AFilter{UnarchivedAFilter()}); err != nil {
			glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error querying pending agreement %v, error: %v", reply.AgreementId(), err)))
		} else if agreement == nil {
			glog.V(5).Infof(BAWlogstring(workerId, fmt.Sprintf("discarding reply, agreement id %v not in our database", reply.AgreementId())))
		} else if cph.AlreadyReceivedReply(agreement) {
			glog.V(5).Infof(BAWlogstring(workerId, fmt.Sprintf("discarding reply, agreement id %v already received a reply", agreement.CurrentAgreementId)))
			// this will cause us to not send a reply ack, which is what we want in this case
			sendReply = false

			// Now we need to write the info to the exchange and the database
		} else if proposal, err := protocolHandler.DemarshalProposal(agreement.Proposal); err != nil {
			glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error validating proposal from pending agreement %v, error: %v", reply.AgreementId(), err)))
		} else if pol, err := policy.DemarshalPolicy(proposal.TsAndCs()); err != nil {
			glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error demarshalling tsandcs policy from pending agreement %v, error: %v", reply.AgreementId(), err)))

		} else if err := cph.PersistReply(reply, pol, workerId); err != nil {
			glog.Errorf(err.Error())

		} else if err := cph.RecordConsumerAgreementState(reply.AgreementId(), pol, agreement.Org, "Producer agreed", b.workerID); err != nil {
			glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error setting agreement state for %v", reply.AgreementId())))

			// We need to send a reply ack and write the info to the blockchain
		} else if consumerPolicy, err := policy.DemarshalPolicy(agreement.Policy); err != nil {
			glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("unable to demarshal policy for agreement %v, error %v", reply.AgreementId(), err)))
		} else {
			// Done handling the response successfully
			ackReplyAsValid = true

			// If we dont have a workload usage record for this device, then we need to create one. If there is already a
			// workload usage record and workload rollback retry counting is enabled, then check to see if the workload priority
			// has changed. If so, update the record and reset the retry count and time. Othwerwise just update the retry count.
			if wlUsage, err := FindSingleWorkloadUsageByDeviceAndPolicyName(b.db, wi.SenderId, consumerPolicy.Header.Name); err != nil {
				glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error searching for persistent workload usage records for device %v with policy %v, error: %v", wi.SenderId, consumerPolicy.Header.Name, err)))
			} else if wlUsage == nil {
				// There is no workload usage record. Make sure that the current workload chosen is the highest priority workload.
				// There could have been a change in the system such that the chosen workload is no longer the right choice. If this
				// is the case, then we need to reject the agreement and start over.

				workload := consumerPolicy.NextHighestPriorityWorkload(0, 0, 0)
				if !workload.Priority.IsSame(pol.Workloads[0].Priority) {
					// Need a new workload usage record but not the same as the highest priority. That can't be right.
					ackReplyAsValid = false
				} else if !pol.Workloads[0].HasEmptyPriority() {
					if err := NewWorkloadUsage(b.db, wi.SenderId, pol.HAGroup.Partners, agreement.Policy, consumerPolicy.Header.Name, pol.Workloads[0].Priority.PriorityValue, pol.Workloads[0].Priority.RetryDurationS, pol.Workloads[0].Priority.VerifiedDurationS, false, reply.AgreementId()); err != nil {
						glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error creating persistent workload usage records for device %v with policy %v, error: %v", wi.SenderId, consumerPolicy.Header.Name, err)))
					}
				}
			} else {
				if wlUsage.Policy == "" {
					if _, err := UpdatePolicy(b.db, wi.SenderId, consumerPolicy.Header.Name, agreement.Policy); err != nil {
						glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error updating policy in workload usage prioroty for device %v with policy %v, error: %v", wi.SenderId, consumerPolicy.Header.Name, err)))
					}
				}

				if !wlUsage.DisableRetry {
					if pol.Workloads[0].Priority.PriorityValue != wlUsage.Priority {
						if _, err := UpdatePriority(b.db, wi.SenderId, consumerPolicy.Header.Name, pol.Workloads[0].Priority.PriorityValue, pol.Workloads[0].Priority.RetryDurationS, pol.Workloads[0].Priority.VerifiedDurationS, reply.AgreementId()); err != nil {
							glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error updating workload usage prioroty for device %v with policy %v, error: %v", wi.SenderId, consumerPolicy.Header.Name, err)))
						}
					} else if _, err := UpdateRetryCount(b.db, wi.SenderId, consumerPolicy.Header.Name, wlUsage.RetryCount+1, reply.AgreementId()); err != nil {
						glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error updating workload usage retry count for device %v with policy %v, error: %v", wi.SenderId, consumerPolicy.Header.Name, err)))
					}
				} else if _, err := UpdateWUAgreementId(b.db, wi.SenderId, consumerPolicy.Header.Name, reply.AgreementId()); err != nil {
					glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error updating agreement id %v in workload usage for %v for policy %v, error: %v", reply.AgreementId(), wi.SenderId, consumerPolicy.Header.Name, err)))
				}
			}

			// Send the reply Ack if it's still valid.
			if ackReplyAsValid {
				if mt, err := exchange.CreateMessageTarget(wi.SenderId, nil, wi.SenderPubKey, wi.From); err != nil {
					glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error creating message target: %v", err)))
				} else if err := protocolHandler.Confirm(ackReplyAsValid, reply.AgreementId(), mt, cph.GetSendMessage()); err != nil {
					glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error trying to send reply ack for %v to %v, error: %v", reply.AgreementId(), mt, err)))
				}

				// Delete the original reply message
				if wi.MessageId != 0 {
					if err := cph.DeleteMessage(wi.MessageId); err != nil {
						glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error deleting message %v from exchange for agbot %v", wi.MessageId, cph.ExchangeId())))
					}
				}

				deletedMessage = true
				droppedLock = true
				lock.Unlock()

				if err := cph.PostReply(reply.AgreementId(), proposal, reply, consumerPolicy, agreement.Org, workerId); err != nil {
					glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error trying to record agreement in blockchain, %v", err)))
					b.CancelAgreementWithLock(cph, reply.AgreementId(), cph.GetTerminationCode(TERM_REASON_CANCEL_BC_WRITE_FAILED), workerId)
					ackReplyAsValid = false
				}

			}
		}

		// Always send an ack for a reply with a positive decision in it
		if !ackReplyAsValid && sendReply {
			if mt, err := exchange.CreateMessageTarget(wi.SenderId, nil, wi.SenderPubKey, wi.From); err != nil {
				glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error creating message target: %v", err)))
			} else if err := protocolHandler.Confirm(ackReplyAsValid, reply.AgreementId(), mt, cph.GetSendMessage()); err != nil {
				glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error trying to send reply ack for %v to %v, error: %v", reply.AgreementId(), wi.From, err)))
			}
		}

	} else {
		glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("received rejection from producer %v", reply)))

		b.CancelAgreement(cph, reply.AgreementId(), cph.GetTerminationCode(TERM_REASON_NEGATIVE_REPLY), workerId)
	}

	// Get rid of the lock
	if !droppedLock {
		lock.Unlock()
	}

	// Get rid of the exchange message if there is one
	if wi.MessageId != 0 && !deletedMessage {
		if err := cph.DeleteMessage(wi.MessageId); err != nil {
			glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error deleting message %v from exchange for agbot %v", wi.MessageId, cph.ExchangeId())))
		}
	}

	return ackReplyAsValid

}

func (b *BaseAgreementWorker) HandleDataReceivedAck(cph ConsumerProtocolHandler, wi *HandleDataReceivedAck, workerId string) {

	protocolHandler := cph.AgreementProtocolHandler("", "", "") // Use the generic protocol handler

	if d, err := protocolHandler.ValidateDataReceivedAck(wi.Ack); err != nil {
		glog.Warningf(BAWlogstring(workerId, fmt.Sprintf("discarding message: %v", wi.Ack)))
	} else if drAck, ok := d.(*abstractprotocol.BaseDataReceivedAck); !ok {
		glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("unable to cast Data Received Ack %v to %v Proposal Reply, is %T", d, cph.Name(), d)))
	} else {

		// Get the agreement id lock to prevent any other thread from processing this same agreement.
		lock := b.alm.getAgreementLock(drAck.AgreementId())
		lock.Lock()

		if ag, err := FindSingleAgreementByAgreementId(b.db, drAck.AgreementId(), cph.Name(), []AFilter{UnarchivedAFilter()}); err != nil {
			glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error querying timed out agreement %v, error: %v", drAck.AgreementId(), err)))
		} else if ag == nil {
			glog.V(3).Infof(BAWlogstring(workerId, fmt.Sprintf("nothing to terminate for agreement %v, no database record.", drAck.AgreementId())))
		} else if _, err := DataNotification(b.db, ag.CurrentAgreementId, cph.Name()); err != nil {
			glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("unable to record data notification, error: %v", err)))
		}

		// Drop the lock. The code block above must always flow through this point.
		lock.Unlock()

	}

	// Get rid of the exchange message if there is one
	if wi.MessageId != 0 {
		if err := cph.DeleteMessage(wi.MessageId); err != nil {
			glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error deleting message %v from exchange for agbot %v", wi.MessageId, cph.ExchangeId())))
		}
	}

}

func (b *BaseAgreementWorker) HandleWorkloadUpgrade(cph ConsumerProtocolHandler, wi *HandleWorkloadUpgrade, workerId string) {

	// Force an upgrade of a workload on a specific device, given a specific policy that delivered the workload.
	// The upgrade request will contain a specific device and policy name, but it might not contain an agreement
	// id. At this point we assume that the originator of the workload upgrade event validated that the agreement id
	// (if specified) matches the device and policy name. Further, the caller has also validated that the device does
	// (or did) have a workload running from the specified policy name.

	// If there is no agreement id specified then find one for the current device and policy name. If we find one,
	// grab the agreement id lock, cancel the agreement and delete the workload usage record.

	if wi.AgreementId == "" {
		if ags, err := FindAgreements(b.db, []AFilter{DevPolAFilter(wi.Device, wi.PolicyName)}, cph.Name()); err != nil {
			glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error finding agreement for device %v and policyName %v, error: %v", wi.Device, wi.PolicyName, err)))
		} else if len(ags) == 0 {
			// If there is no agreement found, is it a problem? We could have caught the system in a state where there is no
			// agreement, but there still might be a workload usage record for the device and policy name. It should be safe to
			// just delete the workload usage record. When an agreement reply is processed, the code will verify that the
			// highest priority workload is being used when creating a new workload usage record.
			glog.V(5).Infof(BAWlogstring(workerId, fmt.Sprintf("forced workload upgrade found no current agreement for device %v and policy name %v", wi.Device, wi.PolicyName)))
		} else {
			// Cancel all agreements
			for _, ag := range ags {
				// Terminate the agreement
				b.CancelAgreementWithLock(cph, ag.CurrentAgreementId, cph.GetTerminationCode(TERM_REASON_CANCEL_FORCED_UPGRADE), workerId)
			}
		}
	} else {
		// Terminate the agreement
		b.CancelAgreementWithLock(cph, wi.AgreementId, cph.GetTerminationCode(TERM_REASON_CANCEL_FORCED_UPGRADE), workerId)
	}

	// Find the workload usage record and delete it. This will cause any new agreement negotiations to start with the highest priority
	// workload.
	if err := DeleteWorkloadUsage(b.db, wi.Device, wi.PolicyName); err != nil {
		glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error deleting workload usage record for device %v and policyName %v, error: %v", wi.Device, wi.PolicyName, err)))
	}

}

func (b *BaseAgreementWorker) CancelAgreementWithLock(cph ConsumerProtocolHandler, agreementId string, reason uint, workerId string) {
	// Get the agreement id lock to prevent any other thread from processing this same agreement.
	lock := b.AgreementLockManager().getAgreementLock(agreementId)
	lock.Lock()

	// Terminate the agreement
	b.CancelAgreement(cph, agreementId, reason, workerId)

	lock.Unlock()

	// Don't need the agreement lock anymore
	b.AgreementLockManager().deleteAgreementLock(agreementId)
}

func (b *BaseAgreementWorker) CancelAgreement(cph ConsumerProtocolHandler, agreementId string, reason uint, workerId string) {

	// Start timing out the agreement
	glog.V(3).Infof(BAWlogstring(workerId, fmt.Sprintf("terminating agreement %v.", agreementId)))

	// Update the database
	if _, err := AgreementTimedout(b.db, agreementId, cph.Name()); err != nil {
		glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error marking agreement %v terminated: %v", agreementId, err)))
	}

	// Update state in exchange
	if err := DeleteConsumerAgreement(b.config.Collaborators.HTTPClientFactory.NewHTTPClient(nil), b.config.AgreementBot.ExchangeURL, cph.ExchangeId(), cph.ExchangeToken(), agreementId); err != nil {
		glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error deleting agreement %v in exchange: %v", agreementId, err)))
	}

	// Find the agreement record
	if ag, err := FindSingleAgreementByAgreementId(b.db, agreementId, cph.Name(), []AFilter{UnarchivedAFilter()}); err != nil {
		glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error querying agreement %v from database, error: %v", agreementId, err)))
	} else if ag == nil {
		glog.V(3).Infof(BAWlogstring(workerId, fmt.Sprintf("nothing to terminate for agreement %v, no database record.", agreementId)))
	} else {

		// Update the workload usage record to clear the agreement. There might not be a workload usage record if there is no workload priority
		// specified in the workload section of the policy.
		if wlUsage, err := UpdateWUAgreementId(b.db, ag.DeviceId, ag.PolicyName, ""); err != nil {
			glog.Warningf(BAWlogstring(workerId, fmt.Sprintf("warning updating agreement id in workload usage for %v for policy %v, error: %v", ag.DeviceId, ag.PolicyName, err)))

		} else if wlUsage != nil && wlUsage.ReqsNotMet {
			// If the workload usage record indicates that it is not at the highest priority workload because the device cant meet the
			// requirements of the higher priority workload, then when an agreement gets cancelled, we will remove the record so that the
			// agbot always tries the next agreement starting with the highest priority workload again.
			if err := DeleteWorkloadUsage(b.db, ag.DeviceId, ag.PolicyName); err != nil {
				glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error deleting workload usage record for device %v and policyName %v, error: %v", ag.DeviceId, ag.PolicyName, err)))
			}
		}

		// Remove the long blockchain cancel from the worker thread. It is important to give the protocol handler a chance to
		// do whatever cleanup and termination it needs to do so we should never skip calling this function.
		// If we can do the termination now, do it. Otherwise we will queue a command to do it later.

		if cph.CanCancelNow(ag) || ag.CounterPartyAddress == "" {
			b.DoAsyncCancel(cph, ag, reason, workerId)
		}

		if ag.AgreementProtocolVersion < 2 || (ag.BlockchainType != "" && !cph.IsBlockchainWritable(ag.BlockchainType, ag.BlockchainName, ag.BlockchainOrg)) {
			// create deferred termination command
			glog.V(3).Infof(BAWlogstring(workerId, fmt.Sprintf("deferring blockchain cancel for %v", agreementId)))
			cph.DeferCommand(AsyncCancelAgreement{
				workType:    ASYNC_CANCEL,
				AgreementId: agreementId,
				Protocol:    cph.Name(),
				Reason:      reason,
			})
		}

		// Archive the record
		if _, err := ArchiveAgreement(b.db, ag.CurrentAgreementId, cph.Name(), reason, cph.GetTerminationReason(reason)); err != nil {
			glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error archiving terminated agreement: %v, error: %v", ag.CurrentAgreementId, err)))
		}

	}
}

func (b *BaseAgreementWorker) ExternalCancel(cph ConsumerProtocolHandler, agreementId string, reason uint, workerId string) {

	glog.V(3).Infof(BAWlogstring(workerId, fmt.Sprintf("starting deferred cancel for %v", agreementId)))

	// Find the agreement record
	if ag, err := FindSingleAgreementByAgreementId(b.db, agreementId, cph.Name(), []AFilter{}); err != nil {
		glog.Errorf(BAWlogstring(workerId, fmt.Sprintf("error querying agreement %v from database, error: %v", agreementId, err)))
	} else if ag == nil {
		glog.V(3).Infof(BAWlogstring(workerId, fmt.Sprintf("nothing to terminate for agreement %v, no database record.", agreementId)))
	} else {
		bcType, bcName, bcOrg := cph.GetKnownBlockchain(ag)
		if cph.IsBlockchainWritable(bcType, bcName, bcOrg) {
			b.DoAsyncCancel(cph, ag, reason, workerId)

		} else {
			glog.V(3).Infof(BAWlogstring(workerId, fmt.Sprintf("deferring blockchain cancel for %v", agreementId)))
			cph.DeferCommand(AsyncCancelAgreement{
				workType:    ASYNC_CANCEL,
				AgreementId: agreementId,
				Protocol:    cph.Name(),
				Reason:      reason,
			})
		}
	}
}

func (b *BaseAgreementWorker) DoAsyncCancel(cph ConsumerProtocolHandler, ag *Agreement, reason uint, workerId string) {

	glog.V(3).Infof(BAWlogstring(workerId, fmt.Sprintf("starting async cancel for %v", ag.CurrentAgreementId)))
	// This routine does not need to be a subworker because it will terminate on its own.
	go cph.TerminateAgreement(ag, reason, workerId)

}

var BAWlogstring = func(workerID string, v interface{}) string {
	return fmt.Sprintf("Base Agreement Worker (%v): %v", workerID, v)
}

// This function checks the Exchange for every declared HA partner to verify that the partner is registered in the
// exchange. As long as all partners are registered, agreements can be made. The partners dont have to be up and heart
// beating, they just have to be registered. If not all partners are registered then no agreements will be attempted
// with any of the registered partners.
func (b *BaseAgreementWorker) incompleteHAGroup(cph ConsumerProtocolHandler, producerPolicy *policy.Policy) error {

	// If the HA group specification is empty, there is nothing to check.
	if len(producerPolicy.HAGroup.Partners) == 0 {
		return nil
	} else {

		// Make sure all partners are in the exchange
		for _, partnerId := range producerPolicy.HAGroup.Partners {

			if _, err := GetDevice(b.config.Collaborators.HTTPClientFactory.NewHTTPClient(nil), partnerId, b.config.AgreementBot.ExchangeURL, cph.ExchangeId(), cph.ExchangeToken()); err != nil {
				return errors.New(fmt.Sprintf("could not obtain device %v from the exchange: %v", partnerId, err))
			}
		}
		return nil

	}
}

// Legacy function. Ignore devices that export specificly known configured properties.
func (b *BaseAgreementWorker) ignoreDevice(pol *policy.Policy) (bool, error) {

	for _, prop := range pol.Properties {
		if listContains(b.config.AgreementBot.IgnoreContractWithAttribs, prop.Name) {
			return true, nil
		}
	}
	return false, nil
}
