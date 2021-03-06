//
// goamz - Go packages to interact with the Amazon Web Services.
//
//   https://wiki.ubuntu.com/goamz
//
// Copyright (c) 2011 Canonical Ltd.
//
// Written by Gustavo Niemeyer <gustavo.niemeyer@canonical.com>
//

package ec2

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goamz/goamz/aws"
)

const debug = false

// The EC2 type encapsulates operations with a specific EC2 region.
type EC2 struct {
	aws.Auth
	aws.Region
	httpClient *http.Client
	private    byte // Reserve the right of using private data.
}

// NewWithClient creates a new EC2 with a custom http client
func NewWithClient(auth aws.Auth, region aws.Region, client *http.Client) *EC2 {
	return &EC2{auth, region, client, 0}
}

// New creates a new EC2.
func New(auth aws.Auth, region aws.Region) *EC2 {
	return NewWithClient(auth, region, aws.RetryingClient)
}

// ----------------------------------------------------------------------------
// Filtering helper.

// Filter builds filtering parameters to be used in an EC2 query which supports
// filtering.  For example:
//
//     filter := NewFilter()
//     filter.Add("architecture", "i386")
//     filter.Add("launch-index", "0")
//     resp, err := ec2.Instances(nil, filter)
//
type Filter struct {
	m map[string][]string
}

// NewFilter creates a new Filter.
func NewFilter() *Filter {
	return &Filter{make(map[string][]string)}
}

// Add appends a filtering parameter with the given name and value(s).
func (f *Filter) Add(name string, value ...string) {
	f.m[name] = append(f.m[name], value...)
}

func (f *Filter) addParams(params map[string]string) {
	if f != nil {
		a := make([]string, len(f.m))
		i := 0
		for k := range f.m {
			a[i] = k
			i++
		}
		sort.StringSlice(a).Sort()
		for i, k := range a {
			prefix := "Filter." + strconv.Itoa(i+1)
			params[prefix+".Name"] = k
			for j, v := range f.m[k] {
				params[prefix+".Value."+strconv.Itoa(j+1)] = v
			}
		}
	}
}

// ----------------------------------------------------------------------------
// Request dispatching logic.

// Error encapsulates an error returned by EC2.
//
// See http://goo.gl/VZGuC for more details.
type Error struct {
	// HTTP status code (200, 403, ...)
	StatusCode int
	// EC2 error code ("UnsupportedOperation", ...)
	Code string
	// The human-oriented error message
	Message   string
	RequestId string `xml:"RequestID"`
}

func (err *Error) Error() string {
	if err.Code == "" {
		return err.Message
	}

	return fmt.Sprintf("%s (%s)", err.Message, err.Code)
}

// For now a single error inst is being exposed. In the future it may be useful
// to provide access to all of them, but rather than doing it as an array/slice,
// use a *next pointer, so that it's backward compatible and it continues to be
// easy to handle the first error, which is what most people will want.
type xmlErrors struct {
	RequestId string  `xml:"RequestID"`
	Errors    []Error `xml:"Errors>Error"`
}

var timeNow = time.Now

func (ec2 *EC2) query(params map[string]string, resp interface{}) error {
	params["Version"] = "2014-02-01"
	params["Timestamp"] = timeNow().In(time.UTC).Format(time.RFC3339)
	endpoint, err := url.Parse(ec2.Region.EC2Endpoint)
	if err != nil {
		return err
	}
	if endpoint.Path == "" {
		endpoint.Path = "/"
	}
	sign(ec2.Auth, "GET", endpoint.Path, params, endpoint.Host)
	endpoint.RawQuery = multimap(params).Encode()
	if debug {
		log.Printf("get { %v } -> {\n", endpoint.String())
	}
	r, err := ec2.httpClient.Get(endpoint.String())
	if err != nil {
		return err
	}
	defer r.Body.Close()

	if debug {
		dump, _ := httputil.DumpResponse(r, true)
		log.Printf("response:\n")
		log.Printf("%v\n}\n", string(dump))
	}
	if r.StatusCode != 200 {
		return buildError(r)
	}
	err = xml.NewDecoder(r.Body).Decode(resp)
	return err
}

func multimap(p map[string]string) url.Values {
	q := make(url.Values, len(p))
	for k, v := range p {
		q[k] = []string{v}
	}
	return q
}

func buildError(r *http.Response) error {
	errors := xmlErrors{}
	xml.NewDecoder(r.Body).Decode(&errors)
	var err Error
	if len(errors.Errors) > 0 {
		err = errors.Errors[0]
	}
	err.RequestId = errors.RequestId
	err.StatusCode = r.StatusCode
	if err.Message == "" {
		err.Message = r.Status
	}
	return &err
}

func makeParams(action string) map[string]string {
	params := make(map[string]string)
	params["Action"] = action
	return params
}

func addParamsList(params map[string]string, label string, ids []string) {
	for i, id := range ids {
		params[label+"."+strconv.Itoa(i+1)] = id
	}
}

func addBlockDeviceParams(prename string, params map[string]string, blockdevices []BlockDeviceMapping) {
	for i, k := range blockdevices {
		// Fixup index since Amazon counts these from 1
		prefix := prename + "BlockDeviceMapping." + strconv.Itoa(i+1) + "."

		if k.DeviceName != "" {
			params[prefix+"DeviceName"] = k.DeviceName
		}
		if k.VirtualName != "" {
			params[prefix+"VirtualName"] = k.VirtualName
		}
		if k.SnapshotId != "" {
			params[prefix+"Ebs.SnapshotId"] = k.SnapshotId
		}
		if k.VolumeType != "" {
			params[prefix+"Ebs.VolumeType"] = k.VolumeType
		}
		if k.IOPS != 0 {
			params[prefix+"Ebs.Iops"] = strconv.FormatInt(k.IOPS, 10)
		}
		if k.VolumeSize != 0 {
			params[prefix+"Ebs.VolumeSize"] = strconv.FormatInt(k.VolumeSize, 10)
		}
		if k.DeleteOnTermination {
			params[prefix+"Ebs.DeleteOnTermination"] = "true"
		}
		if k.NoDevice {
			params[prefix+"NoDevice"] = "true"
		}
	}
}

// ----------------------------------------------------------------------------
// Instance management functions and types.

// RunInstancesOptions encapsulates options for the respective request in EC2.
//
// See http://goo.gl/Mcm3b for more details.
type RunInstancesOptions struct {
	ImageId                  string
	MinCount                 int
	MaxCount                 int
	KeyName                  string
	InstanceType             string
	SecurityGroups           []SecurityGroup
	KernelId                 string
	RamdiskId                string
	UserData                 []byte
	AvailabilityZone         string
	PlacementGroupName       string
	Tenancy                  string
	Monitoring               bool
	SubnetId                 string
	DisableAPITermination    bool
	ShutdownBehavior         string
	PrivateIPAddress         string
	IamInstanceProfile       IamInstanceProfile
	BlockDevices             []BlockDeviceMapping
	EbsOptimized             bool
	AssociatePublicIpAddress bool
}

// Response to a RunInstances request.
//
// See http://goo.gl/Mcm3b for more details.
type RunInstancesResp struct {
	RequestId      string          `xml:"requestId"`
	ReservationId  string          `xml:"reservationId"`
	OwnerId        string          `xml:"ownerId"`
	SecurityGroups []SecurityGroup `xml:"groupSet>item"`
	Instances      []Instance      `xml:"instancesSet>item"`
}

// Instance encapsulates a running instance in EC2.
//
// See http://goo.gl/OCH8a for more details.
type Instance struct {

	// General instance information
	InstanceId         string              `xml:"instanceId"`                 // The ID of the instance launched
	InstanceType       string              `xml:"instanceType"`               // The instance type eg. m1.small | m1.medium | m1.large etc
	AvailabilityZone   string              `xml:"placement>availabilityZone"` // The Availability Zone the instance is located in
	Tags               []Tag               `xml:"tagSet>item"`                // Any tags assigned to the resource
	State              InstanceState       `xml:"instanceState"`              // The current state of the instance
	Reason             string              `xml:"reason"`                     // The reason for the most recent state transition. This might be an empty string
	StateReason        InstanceStateReason `xml:"stateReason"`                // The reason for the most recent state transition
	ImageId            string              `xml:"imageId"`                    // The ID of the AMI used to launch the instance
	KeyName            string              `xml:"keyName"`                    // The key pair name, if this instance was launched with an associated key pair
	Monitoring         string              `xml:"monitoring>state"`           // Valid values: disabled | enabled | pending
	IamInstanceProfile IamInstanceProfile  `xml:"iamInstanceProfile"`         // The IAM instance profile associated with the instance
	LaunchTime         string              `xml:"launchTime"`                 // The time the instance was launched
	OwnerId            string              // This isn't currently returned in the response, and is taken from the parent reservation

	// More specific information
	Architecture          string        `xml:"architecture"`          // Valid values: i386 | x86_64
	Hypervisor            string        `xml:"hypervisor"`            // Valid values: ovm | xen
	KernelId              string        `xml:"kernelId"`              // The kernel associated with this instance
	RamDiskId             string        `xml:"ramdiskId"`             // The RAM disk associated with this instance
	Platform              string        `xml:"platform"`              // The value is Windows for Windows AMIs; otherwise blank
	VirtualizationType    string        `xml:"virtualizationType"`    // Valid values: paravirtual | hvm
	AMILaunchIndex        int           `xml:"amiLaunchIndex"`        // The AMI launch index, which can be used to find this instance in the launch group
	PlacementGroupName    string        `xml:"placement>groupName"`   // The name of the placement group the instance is in (for cluster compute instances)
	Tenancy               string        `xml:"placement>tenancy"`     // (VPC only) Valid values: default | dedicated
	InstanceLifecycle     string        `xml:"instanceLifecycle"`     // Spot instance? Valid values: "spot" or blank
	SpotInstanceRequestId string        `xml:"spotInstanceRequestId"` // The ID of the Spot Instance request
	ClientToken           string        `xml:"clientToken"`           // The idempotency token you provided when you launched the instance
	ProductCodes          []ProductCode `xml:"productCodes>item"`     // The product codes attached to this instance

	// Storage
	RootDeviceType string        `xml:"rootDeviceType"`          // Valid values: ebs | instance-store
	RootDeviceName string        `xml:"rootDeviceName"`          // The root device name (for example, /dev/sda1)
	BlockDevices   []BlockDevice `xml:"blockDeviceMapping>item"` // Any block device mapping entries for the instance
	EbsOptimized   bool          `xml:"ebsOptimized"`            // Indicates whether the instance is optimized for Amazon EBS I/O

	// Network
	DNSName          string          `xml:"dnsName"`          // The public DNS name assigned to the instance. This element remains empty until the instance enters the running state
	PrivateDNSName   string          `xml:"privateDnsName"`   // The private DNS name assigned to the instance. This DNS name can only be used inside the Amazon EC2 network. This element remains empty until the instance enters the running state
	IPAddress        string          `xml:"ipAddress"`        // The public IP address assigned to the instance
	PrivateIPAddress string          `xml:"privateIpAddress"` // The private IP address assigned to the instance
	SubnetId         string          `xml:"subnetId"`         // The ID of the subnet in which the instance is running
	VpcId            string          `xml:"vpcId"`            // The ID of the VPC in which the instance is running
	SecurityGroups   []SecurityGroup `xml:"groupSet>item"`    // A list of the security groups for the instance

	// Advanced Networking
	NetworkInterfaces []InstanceNetworkInterface `xml:"networkInterfaceSet>item"` // (VPC) One or more network interfaces for the instance
	SourceDestCheck   bool                       `xml:"sourceDestCheck"`          // Controls whether source/destination checking is enabled on the instance
	SriovNetSupport   string                     `xml:"sriovNetSupport"`          // Specifies whether enhanced networking is enabled. Valid values: simple
}

// isSpotInstance returns if the instance is a spot instance
func (i Instance) IsSpotInstance() bool {
	if i.InstanceLifecycle == "spot" {
		return true
	}
	return false
}

type BlockDevice struct {
	DeviceName string `xml:"deviceName"`
	EBS        EBS    `xml:"ebs"`
}

type EBS struct {
	VolumeId            string `xml:"volumeId"`
	Status              string `xml:"status"`
	AttachTime          string `xml:"attachTime"`
	DeleteOnTermination bool   `xml:"deleteOnTermination"`
}

// ProductCode represents a product code
// See http://goo.gl/hswmQm for more details.
type ProductCode struct {
	ProductCode string `xml:"productCode"` // The product code
	Type        string `xml:"type"`        // Valid values: devpay | marketplace
}

// InstanceNetworkInterface represents a network interface attached to an instance
// See http://goo.gl/9eW02N for more details.
type InstanceNetworkInterface struct {
	Id                 string                              `xml:"networkInterfaceId"`
	Description        string                              `xml:"description"`
	SubnetId           string                              `xml:"subnetId"`
	VpcId              string                              `xml:"vpcId"`
	OwnerId            string                              `xml:"ownerId"` // The ID of the AWS account that created the network interface.
	Status             string                              `xml:"status"`  // Valid values: available | attaching | in-use | detaching
	MacAddress         string                              `xml:"macAddress"`
	PrivateIPAddress   string                              `xml:"privateIpAddress"`
	PrivateDNSName     string                              `xml:"privateDnsName"`
	SourceDestCheck    bool                                `xml:"sourceDestCheck"`
	SecurityGroups     []SecurityGroup                     `xml:"groupSet>item"`
	Attachment         InstanceNetworkInterfaceAttachment  `xml:"attachment"`
	Association        InstanceNetworkInterfaceAssociation `xml:"association"`
	PrivateIPAddresses []InstancePrivateIpAddress          `xml:"privateIpAddressesSet>item"`
}

// InstanceNetworkInterfaceAttachment describes a network interface attachment to an instance
// See http://goo.gl/0ql0Cg for more details
type InstanceNetworkInterfaceAttachment struct {
	AttachmentID        string `xml:"attachmentID"`        // The ID of the network interface attachment.
	DeviceIndex         int32  `xml:"deviceIndex"`         // The index of the device on the instance for the network interface attachment.
	Status              string `xml:"status"`              // Valid values: attaching | attached | detaching | detached
	AttachTime          string `xml:"attachTime"`          // Time attached, as a Datetime
	DeleteOnTermination bool   `xml:"deleteOnTermination"` // Indicates whether the network interface is deleted when the instance is terminated.
}

// Describes association information for an Elastic IP address.
// See http://goo.gl/YCDdMe for more details
type InstanceNetworkInterfaceAssociation struct {
	PublicIP      string `xml:"publicIp"`      // The address of the Elastic IP address bound to the network interface
	PublicDNSName string `xml:"publicDnsName"` // The public DNS name
	IPOwnerId     string `xml:"ipOwnerId"`     // The ID of the owner of the Elastic IP address
}

// InstancePrivateIpAddress describes a private IP address
// See http://goo.gl/irN646 for more details
type InstancePrivateIpAddress struct {
	PrivateIPAddress string                              `xml:"privateIpAddress"` // The private IP address of the network interface
	PrivateDNSName   string                              `xml:"privateDnsName"`   // The private DNS name
	Primary          bool                                `xml:"primary"`          // Indicates whether this IP address is the primary private IP address of the network interface
	Association      InstanceNetworkInterfaceAssociation `xml:"association"`      // The association information for an Elastic IP address for the network interface
}

// IamInstanceProfile
// See http://goo.gl/PjyijL for more details
type IamInstanceProfile struct {
	ARN  string `xml:"arn"`
	Id   string `xml:"id"`
	Name string `xml:"name"`
}

// RunInstances starts new instances in EC2.
// If options.MinCount and options.MaxCount are both zero, a single instance
// will be started; otherwise if options.MaxCount is zero, options.MinCount
// will be used instead.
//
// See http://goo.gl/Mcm3b for more details.
func (ec2 *EC2) RunInstances(options *RunInstancesOptions) (resp *RunInstancesResp, err error) {
	params := makeParams("RunInstances")
	params["ImageId"] = options.ImageId
	params["InstanceType"] = options.InstanceType
	var min, max int
	if options.MinCount == 0 && options.MaxCount == 0 {
		min = 1
		max = 1
	} else if options.MaxCount == 0 {
		min = options.MinCount
		max = min
	} else {
		min = options.MinCount
		max = options.MaxCount
	}
	params["MinCount"] = strconv.Itoa(min)
	params["MaxCount"] = strconv.Itoa(max)
	token, err := clientToken()
	if err != nil {
		return nil, err
	}
	params["ClientToken"] = token

	if options.KeyName != "" {
		params["KeyName"] = options.KeyName
	}
	if options.KernelId != "" {
		params["KernelId"] = options.KernelId
	}
	if options.RamdiskId != "" {
		params["RamdiskId"] = options.RamdiskId
	}
	if options.UserData != nil {
		userData := make([]byte, b64.EncodedLen(len(options.UserData)))
		b64.Encode(userData, options.UserData)
		params["UserData"] = string(userData)
	}
	if options.AvailabilityZone != "" {
		params["Placement.AvailabilityZone"] = options.AvailabilityZone
	}
	if options.PlacementGroupName != "" {
		params["Placement.GroupName"] = options.PlacementGroupName
	}
	if options.Tenancy != "" {
		params["Placement.Tenancy"] = options.Tenancy
	}
	if options.Monitoring {
		params["Monitoring.Enabled"] = "true"
	}
	if options.SubnetId != "" && options.AssociatePublicIpAddress {
		// If we have a non-default VPC / Subnet specified, we can flag
		// AssociatePublicIpAddress to get a Public IP assigned. By default these are not provided.
		// You cannot specify both SubnetId and the NetworkInterface.0.* parameters though, otherwise
		// you get: Network interfaces and an instance-level subnet ID may not be specified on the same request
		// You also need to attach Security Groups to the NetworkInterface instead of the instance,
		// to avoid: Network interfaces and an instance-level security groups may not be specified on
		// the same request
		params["NetworkInterface.0.DeviceIndex"] = "0"
		params["NetworkInterface.0.AssociatePublicIpAddress"] = "true"
		params["NetworkInterface.0.SubnetId"] = options.SubnetId

		i := 1
		for _, g := range options.SecurityGroups {
			// We only have SecurityGroupId's on NetworkInterface's, no SecurityGroup params.
			if g.Id != "" {
				params["NetworkInterface.0.SecurityGroupId."+strconv.Itoa(i)] = g.Id
				i++
			}
		}
	} else {
		if options.SubnetId != "" {
			params["SubnetId"] = options.SubnetId
		}

		i, j := 1, 1
		for _, g := range options.SecurityGroups {
			if g.Id != "" {
				params["SecurityGroupId."+strconv.Itoa(i)] = g.Id
				i++
			} else {
				params["SecurityGroup."+strconv.Itoa(j)] = g.Name
				j++
			}
		}
	}
	if options.IamInstanceProfile.ARN != "" {
		params["IamInstanceProfile.Arn"] = options.IamInstanceProfile.ARN
	}
	if options.IamInstanceProfile.Name != "" {
		params["IamInstanceProfile.Name"] = options.IamInstanceProfile.Name
	}
	if options.DisableAPITermination {
		params["DisableApiTermination"] = "true"
	}
	if options.ShutdownBehavior != "" {
		params["InstanceInitiatedShutdownBehavior"] = options.ShutdownBehavior
	}
	if options.PrivateIPAddress != "" {
		params["PrivateIpAddress"] = options.PrivateIPAddress
	}
	if options.EbsOptimized {
		params["EbsOptimized"] = "true"
	}

	addBlockDeviceParams("", params, options.BlockDevices)

	resp = &RunInstancesResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}
	return
}

func clientToken() (string, error) {
	// Maximum EC2 client token size is 64 bytes.
	// Each byte expands to two when hex encoded.
	buf := make([]byte, 32)
	_, err := rand.Read(buf)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// ----------------------------------------------------------------------------
// Spot Instance management functions and types.

// The RequestSpotInstances type encapsulates options for the respective request in EC2.
//
// See http://goo.gl/GRZgCD for more details.
type RequestSpotInstances struct {
	SpotPrice                string
	InstanceCount            int
	Type                     string
	ImageId                  string
	KeyName                  string
	InstanceType             string
	SecurityGroups           []SecurityGroup
	IamInstanceProfile       string
	KernelId                 string
	RamdiskId                string
	UserData                 []byte
	AvailZone                string
	PlacementGroupName       string
	Monitoring               bool
	SubnetId                 string
	AssociatePublicIpAddress bool
	PrivateIPAddress         string
	BlockDevices             []BlockDeviceMapping
}

type SpotInstanceSpec struct {
	ImageId                  string
	KeyName                  string
	InstanceType             string
	SecurityGroups           []SecurityGroup
	IamInstanceProfile       string
	KernelId                 string
	RamdiskId                string
	UserData                 []byte
	AvailZone                string
	PlacementGroupName       string
	Monitoring               bool
	SubnetId                 string
	AssociatePublicIpAddress bool
	PrivateIPAddress         string
	BlockDevices             []BlockDeviceMapping
}

type SpotLaunchSpec struct {
	ImageId            string               `xml:"imageId"`
	KeyName            string               `xml:"keyName"`
	InstanceType       string               `xml:"instanceType"`
	SecurityGroups     []SecurityGroup      `xml:"groupSet>item"`
	IamInstanceProfile string               `xml:"iamInstanceProfile"`
	KernelId           string               `xml:"kernelId"`
	RamdiskId          string               `xml:"ramdiskId"`
	PlacementGroupName string               `xml:"placement>groupName"`
	Monitoring         bool                 `xml:"monitoring>enabled"`
	SubnetId           string               `xml:"subnetId"`
	BlockDevices       []BlockDeviceMapping `xml:"blockDeviceMapping>item"`
}

type SpotRequestResult struct {
	SpotRequestId  string         `xml:"spotInstanceRequestId"`
	SpotPrice      string         `xml:"spotPrice"`
	Type           string         `xml:"type"`
	AvailZone      string         `xml:"launchedAvailabilityZone"`
	InstanceId     string         `xml:"instanceId"`
	State          string         `xml:"state"`
	SpotLaunchSpec SpotLaunchSpec `xml:"launchSpecification"`
	CreateTime     string         `xml:"createTime"`
	Tags           []Tag          `xml:"tagSet>item"`
}

// Response to a RequestSpotInstances request.
//
// See http://goo.gl/GRZgCD for more details.
type RequestSpotInstancesResp struct {
	RequestId          string              `xml:"requestId"`
	SpotRequestResults []SpotRequestResult `xml:"spotInstanceRequestSet>item"`
}

// RequestSpotInstances requests a new spot instances in EC2.
func (ec2 *EC2) RequestSpotInstances(options *RequestSpotInstances) (resp *RequestSpotInstancesResp, err error) {
	params := makeParams("RequestSpotInstances")
	prefix := "LaunchSpecification" + "."

	params["SpotPrice"] = options.SpotPrice
	params[prefix+"ImageId"] = options.ImageId
	params[prefix+"InstanceType"] = options.InstanceType

	if options.InstanceCount != 0 {
		params["InstanceCount"] = strconv.Itoa(options.InstanceCount)
	}
	if options.KeyName != "" {
		params[prefix+"KeyName"] = options.KeyName
	}
	if options.KernelId != "" {
		params[prefix+"KernelId"] = options.KernelId
	}
	if options.RamdiskId != "" {
		params[prefix+"RamdiskId"] = options.RamdiskId
	}
	if options.UserData != nil {
		userData := make([]byte, b64.EncodedLen(len(options.UserData)))
		b64.Encode(userData, options.UserData)
		params[prefix+"UserData"] = string(userData)
	}
	if options.AvailZone != "" {
		params[prefix+"Placement.AvailabilityZone"] = options.AvailZone
	}
	if options.PlacementGroupName != "" {
		params[prefix+"Placement.GroupName"] = options.PlacementGroupName
	}
	if options.Monitoring {
		params[prefix+"Monitoring.Enabled"] = "true"
	}
	if options.SubnetId != "" && options.AssociatePublicIpAddress {
		// If we have a non-default VPC / Subnet specified, we can flag
		// AssociatePublicIpAddress to get a Public IP assigned. By default these are not provided.
		// You cannot specify both SubnetId and the NetworkInterface.0.* parameters though, otherwise
		// you get: Network interfaces and an instance-level subnet ID may not be specified on the same request
		// You also need to attach Security Groups to the NetworkInterface instead of the instance,
		// to avoid: Network interfaces and an instance-level security groups may not be specified on
		// the same request
		params[prefix+"NetworkInterface.0.DeviceIndex"] = "0"
		params[prefix+"NetworkInterface.0.AssociatePublicIpAddress"] = "true"
		params[prefix+"NetworkInterface.0.SubnetId"] = options.SubnetId

		i := 1
		for _, g := range options.SecurityGroups {
			// We only have SecurityGroupId's on NetworkInterface's, no SecurityGroup params.
			if g.Id != "" {
				params[prefix+"NetworkInterface.0.SecurityGroupId."+strconv.Itoa(i)] = g.Id
				i++
			}
		}
	} else {
		if options.SubnetId != "" {
			params[prefix+"SubnetId"] = options.SubnetId
		}

		i, j := 1, 1
		for _, g := range options.SecurityGroups {
			if g.Id != "" {
				params[prefix+"SecurityGroupId."+strconv.Itoa(i)] = g.Id
				i++
			} else {
				params[prefix+"SecurityGroup."+strconv.Itoa(j)] = g.Name
				j++
			}
		}
	}
	if options.IamInstanceProfile != "" {
		params[prefix+"IamInstanceProfile.Name"] = options.IamInstanceProfile
	}
	if options.PrivateIPAddress != "" {
		params[prefix+"PrivateIpAddress"] = options.PrivateIPAddress
	}
	addBlockDeviceParams(prefix, params, options.BlockDevices)

	resp = &RequestSpotInstancesResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}
	return
}

// Response to a DescribeSpotInstanceRequests request.
//
// See http://goo.gl/KsKJJk for more details.
type SpotRequestsResp struct {
	RequestId          string              `xml:"requestId"`
	SpotRequestResults []SpotRequestResult `xml:"spotInstanceRequestSet>item"`
}

// DescribeSpotInstanceRequests returns details about spot requests in EC2.  Both parameters
// are optional, and if provided will limit the spot requests returned to those
// matching the given spot request ids or filtering rules.
//
// See http://goo.gl/KsKJJk for more details.
func (ec2 *EC2) DescribeSpotRequests(spotrequestIds []string, filter *Filter) (resp *SpotRequestsResp, err error) {
	params := makeParams("DescribeSpotInstanceRequests")
	addParamsList(params, "SpotInstanceRequestId", spotrequestIds)
	filter.addParams(params)
	resp = &SpotRequestsResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}
	return
}

// Response to a CancelSpotInstanceRequests request.
//
// See http://goo.gl/3BKHj for more details.
type CancelSpotRequestResult struct {
	SpotRequestId string `xml:"spotInstanceRequestId"`
	State         string `xml:"state"`
}
type CancelSpotRequestsResp struct {
	RequestId                string                    `xml:"requestId"`
	CancelSpotRequestResults []CancelSpotRequestResult `xml:"spotInstanceRequestSet>item"`
}

// CancelSpotRequests requests the cancellation of spot requests when the given ids.
//
// See http://goo.gl/3BKHj for more details.
func (ec2 *EC2) CancelSpotRequests(spotrequestIds []string) (resp *CancelSpotRequestsResp, err error) {
	params := makeParams("CancelSpotInstanceRequests")
	addParamsList(params, "SpotInstanceRequestId", spotrequestIds)
	resp = &CancelSpotRequestsResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}
	return
}

// Response to a TerminateInstances request.
//
// See http://goo.gl/3BKHj for more details.
type TerminateInstancesResp struct {
	RequestId    string                `xml:"requestId"`
	StateChanges []InstanceStateChange `xml:"instancesSet>item"`
}

// InstanceState encapsulates the state of an instance in EC2.
//
// See http://goo.gl/y3ZBq for more details.
type InstanceState struct {
	Code int    `xml:"code"` // Watch out, bits 15-8 have unpublished meaning.
	Name string `xml:"name"`
}

// InstanceStateChange informs of the previous and current states
// for an instance when a state change is requested.
type InstanceStateChange struct {
	InstanceId    string        `xml:"instanceId"`
	CurrentState  InstanceState `xml:"currentState"`
	PreviousState InstanceState `xml:"previousState"`
}

// InstanceStateReason describes a state change for an instance in EC2
//
// See http://goo.gl/KZkbXi for more details
type InstanceStateReason struct {
	Code    string `xml:"code"`
	Message string `xml:"message"`
}

// TerminateInstances requests the termination of instances when the given ids.
//
// See http://goo.gl/3BKHj for more details.
func (ec2 *EC2) TerminateInstances(instIds []string) (resp *TerminateInstancesResp, err error) {
	params := makeParams("TerminateInstances")
	addParamsList(params, "InstanceId", instIds)
	resp = &TerminateInstancesResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}
	return
}

// Response to a DescribeInstances request.
//
// See http://goo.gl/mLbmw for more details.
type DescribeInstancesResp struct {
	RequestId    string        `xml:"requestId"`
	Reservations []Reservation `xml:"reservationSet>item"`
}

// Reservation represents details about a reservation in EC2.
//
// See http://goo.gl/0ItPT for more details.
type Reservation struct {
	ReservationId  string          `xml:"reservationId"`
	OwnerId        string          `xml:"ownerId"`
	RequesterId    string          `xml:"requesterId"`
	SecurityGroups []SecurityGroup `xml:"groupSet>item"`
	Instances      []Instance      `xml:"instancesSet>item"`
}

// Instances returns details about instances in EC2.  Both parameters
// are optional, and if provided will limit the instances returned to those
// matching the given instance ids or filtering rules.
//
// See http://goo.gl/4No7c for more details.
func (ec2 *EC2) DescribeInstances(instIds []string, filter *Filter) (resp *DescribeInstancesResp, err error) {
	params := makeParams("DescribeInstances")
	addParamsList(params, "InstanceId", instIds)
	filter.addParams(params)
	resp = &DescribeInstancesResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}

	// Add additional parameters to instances which aren't available in the response
	for i, rsv := range resp.Reservations {
		ownerId := rsv.OwnerId
		for j, inst := range rsv.Instances {
			inst.OwnerId = ownerId
			resp.Reservations[i].Instances[j] = inst
		}
	}

	return
}

// DescribeInstanceStatusOptions encapsulates the query parameters for the corresponding action.
//
// See http:////goo.gl/2FBTdS for more details.
type DescribeInstanceStatusOptions struct {
	InstanceIds         []string // If non-empty, limit the query to this subset of instances. Maximum length of 100.
	IncludeAllInstances bool     // If true, describe all instances, instead of just running instances (the default).
	MaxResults          int      // Maximum number of results to return. Minimum of 5. Maximum of 1000.
	NextToken           string   // The token for the next set of items to return. (You received this token from a prior call.)
}

// Response to a DescribeInstanceStatus request.
//
// See http://goo.gl/2FBTdS for more details.
type DescribeInstanceStatusResp struct {
	RequestId         string               `xml:"requestId"`
	InstanceStatusSet []InstanceStatusItem `xml:"instanceStatusSet>item"`
	NextToken         string               `xml:"nextToken"`
}

// InstanceStatusItem describes the instance status, cause, details, and potential actions to take in response.
//
// See http://goo.gl/oImFZZ for more details.
type InstanceStatusItem struct {
	InstanceId       string                `xml:"instanceId"`
	AvailabilityZone string                `xml:"availabilityZone"`
	Events           []InstanceStatusEvent `xml:"eventsSet>item"` // Extra information regarding events associated with the instance.
	InstanceState    InstanceState         `xml:"instanceState"`  // The intended state of the instance. Calls to DescribeInstanceStatus require that an instance be in the running state.
	SystemStatus     InstanceStatus        `xml:"systemStatus"`
	InstanceStatus   InstanceStatus        `xml:"instanceStatus"`
}

// InstanceStatusEvent describes an instance event.
//
// See http://goo.gl/PXsDTn for more details.
type InstanceStatusEvent struct {
	Code        string `xml:"code"`        // The associated code of the event.
	Description string `xml:"description"` // A description of the event.
	NotBefore   string `xml:"notBefore"`   // The earliest scheduled start time for the event.
	NotAfter    string `xml:"notAfter"`    // The latest scheduled end time for the event.
}

// InstanceStatus describes the status of an instance with details.
//
// See http://goo.gl/eFch4S for more details.
type InstanceStatus struct {
	Status  string                `xml:"status"`  // The instance status.
	Details InstanceStatusDetails `xml:"details"` // The system instance health or application instance health.
}

// InstanceStatusDetails describes the instance status with the cause and more detail.
//
// See http://goo.gl/3qoMC4 for more details.
type InstanceStatusDetails struct {
	Name          string `xml:"name"`          // The type of instance status.
	Status        string `xml:"status"`        // The status.
	ImpairedSince string `xml:"impairedSince"` // The time when a status check failed. For an instance that was launched and impaired, this is the time when the instance was launched.
}

// DescribeInstanceStatus returns instance status information about instances in EC2.
// instIds and filter are optional, and if provided will limit the instances returned to those
// matching the given instance ids or filtering rules.
// all determines whether to report all matching instances or only those in the running state
//
// See http://goo.gl/2FBTdS for more details.
func (ec2 *EC2) DescribeInstanceStatus(options *DescribeInstanceStatusOptions, filter *Filter) (resp *DescribeInstanceStatusResp, err error) {
	params := makeParams("DescribeInstanceStatus")
	if len(options.InstanceIds) > 0 {
		addParamsList(params, "InstanceId", options.InstanceIds)
	}
	if options.IncludeAllInstances {
		params["IncludeAllInstances"] = "true"
	}
	if options.MaxResults != 0 {
		params["MaxResults"] = strconv.Itoa(options.MaxResults)
	}
	if options.NextToken != "" {
		params["NextToken"] = options.NextToken
	}
	filter.addParams(params)
	resp = &DescribeInstanceStatusResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}
	return
}

// ----------------------------------------------------------------------------
// KeyPair management functions and types.

type CreateKeyPairResp struct {
	RequestId      string `xml:"requestId"`
	KeyName        string `xml:"keyName"`
	KeyFingerprint string `xml:"keyFingerprint"`
	KeyMaterial    string `xml:"keyMaterial"`
}

// CreateKeyPair creates a new key pair and returns the private key contents.
//
// See http://goo.gl/0S6hV
func (ec2 *EC2) CreateKeyPair(keyName string) (resp *CreateKeyPairResp, err error) {
	params := makeParams("CreateKeyPair")
	params["KeyName"] = keyName

	resp = &CreateKeyPairResp{}
	err = ec2.query(params, resp)
	if err == nil {
		resp.KeyFingerprint = strings.TrimSpace(resp.KeyFingerprint)
	}
	return
}

// DeleteKeyPair deletes a key pair.
//
// See http://goo.gl/0bqok
func (ec2 *EC2) DeleteKeyPair(name string) (resp *SimpleResp, err error) {
	params := makeParams("DeleteKeyPair")
	params["KeyName"] = name

	resp = &SimpleResp{}
	err = ec2.query(params, resp)
	return
}

type ImportKeyPairOptions struct {
	KeyName           string
	PublicKeyMaterial string
}

type ImportKeyPairResp struct {
	RequestId      string `xml:"requestId"`
	KeyName        string `xml:"keyName"`
	KeyFingerprint string `xml:"keyFingerprint"`
}

// ImportKeyPair import a key pair.
//
// See http://goo.gl/xpTccS
func (ec2 *EC2) ImportKeyPair(options *ImportKeyPairOptions) (resp *ImportKeyPairResp, err error) {
	params := makeParams("ImportKeyPair")
	params["KeyName"] = options.KeyName
	params["PublicKeyMaterial"] = options.PublicKeyMaterial

	resp = &ImportKeyPairResp{}
	err = ec2.query(params, resp)
	return
}

// ResourceTag represents key-value metadata used to classify and organize
// EC2 instances.
//
// See http://goo.gl/bncl3 for more details
type Tag struct {
	Key   string `xml:"key"`
	Value string `xml:"value"`
}

// CreateTags adds or overwrites one or more tags for the specified taggable resources.
// For a list of tagable resources, see: http://docs.aws.amazon.com/AWSEC2/latest/UserGuide/Using_Tags.html
//
// See http://goo.gl/Vmkqc for more details
func (ec2 *EC2) CreateTags(resourceIds []string, tags []Tag) (resp *SimpleResp, err error) {
	params := makeParams("CreateTags")
	addParamsList(params, "ResourceId", resourceIds)

	for j, tag := range tags {
		params["Tag."+strconv.Itoa(j+1)+".Key"] = tag.Key
		params["Tag."+strconv.Itoa(j+1)+".Value"] = tag.Value
	}

	resp = &SimpleResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// Response to a StartInstances request.
//
// See http://goo.gl/awKeF for more details.
type StartInstanceResp struct {
	RequestId    string                `xml:"requestId"`
	StateChanges []InstanceStateChange `xml:"instancesSet>item"`
}

// Response to a StopInstances request.
//
// See http://goo.gl/436dJ for more details.
type StopInstanceResp struct {
	RequestId    string                `xml:"requestId"`
	StateChanges []InstanceStateChange `xml:"instancesSet>item"`
}

// StartInstances starts an Amazon EBS-backed AMI that you've previously stopped.
//
// See http://goo.gl/awKeF for more details.
func (ec2 *EC2) StartInstances(ids ...string) (resp *StartInstanceResp, err error) {
	params := makeParams("StartInstances")
	addParamsList(params, "InstanceId", ids)
	resp = &StartInstanceResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// StopInstances requests stopping one or more Amazon EBS-backed instances.
//
// See http://goo.gl/436dJ for more details.
func (ec2 *EC2) StopInstances(ids ...string) (resp *StopInstanceResp, err error) {
	params := makeParams("StopInstances")
	addParamsList(params, "InstanceId", ids)
	resp = &StopInstanceResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// RebootInstance requests a reboot of one or more instances. This operation is asynchronous;
// it only queues a request to reboot the specified instance(s). The operation will succeed
// if the instances are valid and belong to you.
//
// Requests to reboot terminated instances are ignored.
//
// See http://goo.gl/baoUf for more details.
func (ec2 *EC2) RebootInstances(ids ...string) (resp *SimpleResp, err error) {
	params := makeParams("RebootInstances")
	addParamsList(params, "InstanceId", ids)
	resp = &SimpleResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// The ModifyInstanceAttribute request parameters.
type ModifyInstance struct {
	InstanceType          string
	BlockDevices          []BlockDeviceMapping
	DisableAPITermination bool
	EbsOptimized          bool
	SecurityGroups        []SecurityGroup
	ShutdownBehavior      string
	KernelId              string
	RamdiskId             string
	SourceDestCheck       bool
	SriovNetSupport       bool
	UserData              []byte
}

// Response to a ModifyInstanceAttribute request.
//
// http://goo.gl/icuXh5 for more details.
type ModifyInstanceResp struct {
	RequestId string `xml:"requestId"`
	Return    bool   `xml:"return"`
}

// ModifyImageAttribute modifies the specified attribute of the specified instance.
// You can specify only one attribute at a time. To modify some attributes, the
// instance must be stopped.
//
// See http://goo.gl/icuXh5 for more details.
func (ec2 *EC2) ModifyInstance(instId string, options *ModifyInstance) (resp *ModifyInstanceResp, err error) {
	params := makeParams("ModifyInstanceAttribute")
	params["InstanceId"] = instId
	addBlockDeviceParams("", params, options.BlockDevices)

	if options.InstanceType != "" {
		params["InstanceType.Value"] = options.InstanceType
	}

	if options.DisableAPITermination {
		params["DisableApiTermination.Value"] = "true"
	}

	if options.EbsOptimized {
		params["EbsOptimized"] = "true"
	}

	if options.ShutdownBehavior != "" {
		params["InstanceInitiatedShutdownBehavior.Value"] = options.ShutdownBehavior
	}

	if options.KernelId != "" {
		params["Kernel.Value"] = options.KernelId
	}

	if options.RamdiskId != "" {
		params["Ramdisk.Value"] = options.RamdiskId
	}

	if options.SourceDestCheck {
		params["SourceDestCheck.Value"] = "true"
	}

	if options.SriovNetSupport {
		params["SriovNetSupport.Value"] = "simple"
	}

	if options.UserData != nil {
		userData := make([]byte, b64.EncodedLen(len(options.UserData)))
		b64.Encode(userData, options.UserData)
		params["UserData"] = string(userData)
	}

	i := 1
	for _, g := range options.SecurityGroups {
		if g.Id != "" {
			params["GroupId."+strconv.Itoa(i)] = g.Id
			i++
		}
	}

	resp = &ModifyInstanceResp{}
	err = ec2.query(params, resp)
	if err != nil {
		resp = nil
	}
	return
}

// Reserved Instances

// Structures

// DescribeReservedInstancesResponse structure returned from a DescribeReservedInstances request.
//
// See
type DescribeReservedInstancesResponse struct {
	RequestId         string                          `xml:"requestId"`
	ReservedInstances []ReservedInstancesResponseItem `xml:"reservedInstancesSet>item"`
}

//
//
// See
type ReservedInstancesResponseItem struct {
	ReservedInstanceId string            `xml:"reservedInstancesId"`
	InstanceType       string            `xml:"instanceType"`
	AvailabilityZone   string            `xml:"availabilityZone"`
	Start              string            `xml:"start"`
	Duration           uint64            `xml:"duration"`
	End                string            `xml:"end"`
	FixedPrice         float32           `xml:"fixedPrice"`
	UsagePrice         float32           `xml:"usagePrice"`
	InstanceCount      int               `xml:"instanceCount"`
	ProductDescription string            `xml:"productDescription"`
	State              string            `xml:"state"`
	Tags               []Tag             `xml:"tagSet->item"`
	InstanceTenancy    string            `xml:"instanceTenancy"`
	CurrencyCode       string            `xml:"currencyCode"`
	OfferingType       string            `xml:"offeringType"`
	RecurringCharges   []RecurringCharge `xml:"recurringCharges>item"`
}

//
//
// See
type RecurringCharge struct {
	Frequency string  `xml:"frequency"`
	Amount    float32 `xml:"amount"`
}

// functions
// DescribeReservedInstances
//
// See
func (ec2 *EC2) DescribeReservedInstances(instIds []string, filter *Filter) (resp *DescribeReservedInstancesResponse, err error) {
	params := makeParams("DescribeReservedInstances")

	for i, id := range instIds {
		params["ReservedInstancesId."+strconv.Itoa(i+1)] = id
	}
	filter.addParams(params)

	resp = &DescribeReservedInstancesResponse{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// ----------------------------------------------------------------------------
// Image and snapshot management functions and types.

// The CreateImage request parameters.
//
// See http://goo.gl/cxU41 for more details.
type CreateImage struct {
	InstanceId   string
	Name         string
	Description  string
	NoReboot     bool
	BlockDevices []BlockDeviceMapping
}

// Response to a CreateImage request.
//
// See http://goo.gl/cxU41 for more details.
type CreateImageResp struct {
	RequestId string `xml:"requestId"`
	ImageId   string `xml:"imageId"`
}

// Response to a DescribeImages request.
//
// See http://goo.gl/hLnyg for more details.
type ImagesResp struct {
	RequestId string  `xml:"requestId"`
	Images    []Image `xml:"imagesSet>item"`
}

// Response to a DescribeImageAttribute request.
//
// See http://goo.gl/bHO3zT for more details.
type ImageAttributeResp struct {
	RequestId    string               `xml:"requestId"`
	ImageId      string               `xml:"imageId"`
	Kernel       string               `xml:"kernel>value"`
	RamDisk      string               `xml:"ramdisk>value"`
	Description  string               `xml:"description>value"`
	Group        string               `xml:"launchPermission>item>group"`
	UserIds      []string             `xml:"launchPermission>item>userId"`
	ProductCodes []string             `xml:"productCodes>item>productCode"`
	BlockDevices []BlockDeviceMapping `xml:"blockDeviceMapping>item"`
}

// The RegisterImage request parameters.
type RegisterImage struct {
	ImageLocation  string
	Name           string
	Description    string
	Architecture   string
	KernelId       string
	RamdiskId      string
	RootDeviceName string
	VirtType       string
	BlockDevices   []BlockDeviceMapping
}

// Response to a RegisterImage request.
type RegisterImageResp struct {
	RequestId string `xml:"requestId"`
	ImageId   string `xml:"imageId"`
}

// Response to a DegisterImage request.
//
// See http://docs.aws.amazon.com/AWSEC2/latest/APIReference/ApiReference-query-DeregisterImage.html
type DeregisterImageResp struct {
	RequestId string `xml:"requestId"`
	Return    bool   `xml:"return"`
}

// BlockDeviceMapping represents the association of a block device with an image.
//
// See http://goo.gl/wnDBf for more details.
type BlockDeviceMapping struct {
	DeviceName          string `xml:"deviceName"`
	VirtualName         string `xml:"virtualName"`
	SnapshotId          string `xml:"ebs>snapshotId"`
	VolumeType          string `xml:"ebs>volumeType"`
	VolumeSize          int64  `xml:"ebs>volumeSize"`
	DeleteOnTermination bool   `xml:"ebs>deleteOnTermination"`
	NoDevice            bool   `xml:"noDevice"`

	// The number of I/O operations per second (IOPS) that the volume supports.
	IOPS int64 `xml:"ebs>iops"`
}

// Image represents details about an image.
//
// See http://goo.gl/iSqJG for more details.
type Image struct {
	Id                 string               `xml:"imageId"`
	Name               string               `xml:"name"`
	Description        string               `xml:"description"`
	Type               string               `xml:"imageType"`
	State              string               `xml:"imageState"`
	Location           string               `xml:"imageLocation"`
	Public             bool                 `xml:"isPublic"`
	Architecture       string               `xml:"architecture"`
	Platform           string               `xml:"platform"`
	ProductCodes       []string             `xml:"productCode>item>productCode"`
	KernelId           string               `xml:"kernelId"`
	RamdiskId          string               `xml:"ramdiskId"`
	StateReason        string               `xml:"stateReason"`
	OwnerId            string               `xml:"imageOwnerId"`
	OwnerAlias         string               `xml:"imageOwnerAlias"`
	RootDeviceType     string               `xml:"rootDeviceType"`
	RootDeviceName     string               `xml:"rootDeviceName"`
	VirtualizationType string               `xml:"virtualizationType"`
	Hypervisor         string               `xml:"hypervisor"`
	BlockDevices       []BlockDeviceMapping `xml:"blockDeviceMapping>item"`
	Tags               []Tag                `xml:"tagSet>item"`
}

// The ModifyImageAttribute request parameters.
type ModifyImageAttribute struct {
	AddUsers     []string
	RemoveUsers  []string
	AddGroups    []string
	RemoveGroups []string
	ProductCodes []string
	Description  string
}

// The CopyImage request parameters.
//
// See http://goo.gl/hQwPCK for more details.
type CopyImage struct {
	SourceRegion  string
	SourceImageId string
	Name          string
	Description   string
	ClientToken   string
}

// Response to a CopyImage request.
//
// See http://goo.gl/hQwPCK for more details.
type CopyImageResp struct {
	RequestId string `xml:"requestId"`
	ImageId   string `xml:"imageId"`
}

// Creates an Amazon EBS-backed AMI from an Amazon EBS-backed instance
// that is either running or stopped.
//
// See http://goo.gl/cxU41 for more details.
func (ec2 *EC2) CreateImage(options *CreateImage) (resp *CreateImageResp, err error) {
	params := makeParams("CreateImage")
	params["InstanceId"] = options.InstanceId
	params["Name"] = options.Name
	if options.Description != "" {
		params["Description"] = options.Description
	}
	if options.NoReboot {
		params["NoReboot"] = "true"
	}
	addBlockDeviceParams("", params, options.BlockDevices)

	resp = &CreateImageResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}

	return
}

// Images returns details about available images.
// The ids and filter parameters, if provided, will limit the images returned.
// For example, to get all the private images associated with this account set
// the boolean filter "is-public" to 0.
// For list of filters: http://docs.aws.amazon.com/AWSEC2/latest/APIReference/ApiReference-query-DescribeImages.html
//
// Note: calling this function with nil ids and filter parameters will result in
// a very large number of images being returned.
//
// See http://goo.gl/SRBhW for more details.
func (ec2 *EC2) Images(ids []string, filter *Filter) (resp *ImagesResp, err error) {
	params := makeParams("DescribeImages")
	for i, id := range ids {
		params["ImageId."+strconv.Itoa(i+1)] = id
	}
	filter.addParams(params)

	resp = &ImagesResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}
	return
}

// ImagesByOwners returns details about available images.
// The ids, owners, and filter parameters, if provided, will limit the images returned.
// For example, to get all the private images associated with this account set
// the boolean filter "is-public" to 0.
// For list of filters: http://docs.aws.amazon.com/AWSEC2/latest/APIReference/ApiReference-query-DescribeImages.html
//
// Note: calling this function with nil ids and filter parameters will result in
// a very large number of images being returned.
//
// See http://goo.gl/SRBhW for more details.
func (ec2 *EC2) ImagesByOwners(ids []string, owners []string, filter *Filter) (resp *ImagesResp, err error) {
	params := makeParams("DescribeImages")
	for i, id := range ids {
		params["ImageId."+strconv.Itoa(i+1)] = id
	}
	for i, owner := range owners {
		params[fmt.Sprintf("Owner.%d", i+1)] = owner
	}

	filter.addParams(params)

	resp = &ImagesResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}
	return
}

// ImageAttribute describes an attribute of an AMI.
// You can specify only one attribute at a time.
// Valid attributes are:
//    description | kernel | ramdisk | launchPermission | productCodes | blockDeviceMapping
//
// See http://goo.gl/bHO3zT for more details.
func (ec2 *EC2) ImageAttribute(imageId, attribute string) (resp *ImageAttributeResp, err error) {
	params := makeParams("DescribeImageAttribute")
	params["ImageId"] = imageId
	params["Attribute"] = attribute

	resp = &ImageAttributeResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}
	return
}

// ModifyImageAttribute sets attributes for an image.
//
// See http://goo.gl/YUjO4G for more details.
func (ec2 *EC2) ModifyImageAttribute(imageId string, options *ModifyImageAttribute) (resp *SimpleResp, err error) {
	params := makeParams("ModifyImageAttribute")
	params["ImageId"] = imageId
	if options.Description != "" {
		params["Description.Value"] = options.Description
	}

	if options.AddUsers != nil {
		for i, user := range options.AddUsers {
			p := fmt.Sprintf("LaunchPermission.Add.%d.UserId", i+1)
			params[p] = user
		}
	}

	if options.RemoveUsers != nil {
		for i, user := range options.RemoveUsers {
			p := fmt.Sprintf("LaunchPermission.Remove.%d.UserId", i+1)
			params[p] = user
		}
	}

	if options.AddGroups != nil {
		for i, group := range options.AddGroups {
			p := fmt.Sprintf("LaunchPermission.Add.%d.Group", i+1)
			params[p] = group
		}
	}

	if options.RemoveGroups != nil {
		for i, group := range options.RemoveGroups {
			p := fmt.Sprintf("LaunchPermission.Remove.%d.Group", i+1)
			params[p] = group
		}
	}

	if options.ProductCodes != nil {
		addParamsList(params, "ProductCode", options.ProductCodes)
	}

	resp = &SimpleResp{}
	err = ec2.query(params, resp)
	if err != nil {
		resp = nil
	}

	return
}

// Registers a new AMI with EC2.
//
// See: http://docs.aws.amazon.com/AWSEC2/latest/APIReference/ApiReference-query-RegisterImage.html
func (ec2 *EC2) RegisterImage(options *RegisterImage) (resp *RegisterImageResp, err error) {
	params := makeParams("RegisterImage")
	params["Name"] = options.Name
	if options.ImageLocation != "" {
		params["ImageLocation"] = options.ImageLocation
	}

	if options.Description != "" {
		params["Description"] = options.Description
	}

	if options.Architecture != "" {
		params["Architecture"] = options.Architecture
	}

	if options.KernelId != "" {
		params["KernelId"] = options.KernelId
	}

	if options.RamdiskId != "" {
		params["RamdiskId"] = options.RamdiskId
	}

	if options.RootDeviceName != "" {
		params["RootDeviceName"] = options.RootDeviceName
	}

	if options.VirtType != "" {
		params["VirtualizationType"] = options.VirtType
	}

	addBlockDeviceParams("", params, options.BlockDevices)

	resp = &RegisterImageResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}

	return
}

// Degisters an image. Note that this does not delete the backing stores of the AMI.
//
// See http://docs.aws.amazon.com/AWSEC2/latest/APIReference/ApiReference-query-DeregisterImage.html
func (ec2 *EC2) DeregisterImage(imageId string) (resp *DeregisterImageResp, err error) {
	params := makeParams("DeregisterImage")
	params["ImageId"] = imageId

	resp = &DeregisterImageResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}

	return
}

// Copy and Image from one region to another.
//
// See http://goo.gl/hQwPCK for more details.
func (ec2 *EC2) CopyImage(options *CopyImage) (resp *CopyImageResp, err error) {
	params := makeParams("CopyImage")

	if options.SourceRegion != "" {
		params["SourceRegion"] = options.SourceRegion
	}

	if options.SourceImageId != "" {
		params["SourceImageId"] = options.SourceImageId
	}

	if options.Name != "" {
		params["Name"] = options.Name
	}

	if options.Description != "" {
		params["Description"] = options.Description
	}

	if options.ClientToken != "" {
		params["ClientToken"] = options.ClientToken
	}

	resp = &CopyImageResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}

	return
}

// Response to a CreateSnapshot request.
//
// See http://goo.gl/ttcda for more details.
type CreateSnapshotResp struct {
	RequestId string `xml:"requestId"`
	Snapshot
}

// CreateSnapshot creates a volume snapshot and stores it in S3.
//
// See http://goo.gl/ttcda for more details.
func (ec2 *EC2) CreateSnapshot(volumeId, description string) (resp *CreateSnapshotResp, err error) {
	params := makeParams("CreateSnapshot")
	params["VolumeId"] = volumeId
	params["Description"] = description

	resp = &CreateSnapshotResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}
	return
}

// DeleteSnapshots deletes the volume snapshots with the given ids.
//
// Note: If you make periodic snapshots of a volume, the snapshots are
// incremental so that only the blocks on the device that have changed
// since your last snapshot are incrementally saved in the new snapshot.
// Even though snapshots are saved incrementally, the snapshot deletion
// process is designed so that you need to retain only the most recent
// snapshot in order to restore the volume.
//
// See http://goo.gl/vwU1y for more details.
func (ec2 *EC2) DeleteSnapshots(ids []string) (resp *SimpleResp, err error) {
	params := makeParams("DeleteSnapshot")
	for i, id := range ids {
		params["SnapshotId."+strconv.Itoa(i+1)] = id
	}

	resp = &SimpleResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}

	return
}

// Response to a DescribeSnapshots request.
//
// See http://goo.gl/nClDT for more details.
type SnapshotsResp struct {
	RequestId string     `xml:"requestId"`
	Snapshots []Snapshot `xml:"snapshotSet>item"`
}

// Snapshot represents details about a volume snapshot.
//
// See http://goo.gl/nkovs for more details.
type Snapshot struct {
	Id          string `xml:"snapshotId"`
	VolumeId    string `xml:"volumeId"`
	VolumeSize  string `xml:"volumeSize"`
	Status      string `xml:"status"`
	StartTime   string `xml:"startTime"`
	Description string `xml:"description"`
	Progress    string `xml:"progress"`
	OwnerId     string `xml:"ownerId"`
	OwnerAlias  string `xml:"ownerAlias"`
	Tags        []Tag  `xml:"tagSet>item"`
}

// Snapshots returns details about volume snapshots available to the user.
// The ids and filter parameters, if provided, limit the snapshots returned.
//
// See http://goo.gl/ogJL4 for more details.
func (ec2 *EC2) Snapshots(ids []string, filter *Filter) (resp *SnapshotsResp, err error) {
	params := makeParams("DescribeSnapshots")
	for i, id := range ids {
		params["SnapshotId."+strconv.Itoa(i+1)] = id
	}
	filter.addParams(params)

	resp = &SnapshotsResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}
	return
}

// ----------------------------------------------------------------------------
// Volume management

// The CreateVolume request parameters
//
// See http://docs.aws.amazon.com/AWSEC2/latest/APIReference/ApiReference-query-CreateVolume.html
type CreateVolume struct {
	AvailZone  string
	Size       int64
	SnapshotId string
	VolumeType string
	IOPS       int64
}

// Response to an AttachVolume request
type AttachVolumeResp struct {
	RequestId  string `xml:"requestId"`
	VolumeId   string `xml:"volumeId"`
	InstanceId string `xml:"instanceId"`
	Device     string `xml:"device"`
	Status     string `xml:"status"`
	AttachTime string `xml:"attachTime"`
}

// Response to a CreateVolume request
type CreateVolumeResp struct {
	RequestId  string `xml:"requestId"`
	VolumeId   string `xml:"volumeId"`
	Size       int64  `xml:"size"`
	SnapshotId string `xml:"snapshotId"`
	AvailZone  string `xml:"availabilityZone"`
	Status     string `xml:"status"`
	CreateTime string `xml:"createTime"`
	VolumeType string `xml:"volumeType"`
	IOPS       int64  `xml:"iops"`
}

// Volume is a single volume.
type Volume struct {
	VolumeId    string             `xml:"volumeId"`
	Size        string             `xml:"size"`
	SnapshotId  string             `xml:"snapshotId"`
	AvailZone   string             `xml:"availabilityZone"`
	Status      string             `xml:"status"`
	Attachments []VolumeAttachment `xml:"attachmentSet>item"`
	VolumeType  string             `xml:"volumeType"`
	IOPS        int64              `xml:"iops"`
	Tags        []Tag              `xml:"tagSet>item"`
}

type VolumeAttachment struct {
	VolumeId   string `xml:"volumeId"`
	InstanceId string `xml:"instanceId"`
	Device     string `xml:"device"`
	Status     string `xml:"status"`
}

// Response to a DescribeVolumes request
type VolumesResp struct {
	RequestId string   `xml:"requestId"`
	Volumes   []Volume `xml:"volumeSet>item"`
}

// Attach a volume.
func (ec2 *EC2) AttachVolume(volumeId string, instanceId string, device string) (resp *AttachVolumeResp, err error) {
	params := makeParams("AttachVolume")
	params["VolumeId"] = volumeId
	params["InstanceId"] = instanceId
	params["Device"] = device

	resp = &AttachVolumeResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

// Create a new volume.
func (ec2 *EC2) CreateVolume(options *CreateVolume) (resp *CreateVolumeResp, err error) {
	params := makeParams("CreateVolume")
	params["AvailabilityZone"] = options.AvailZone
	if options.Size > 0 {
		params["Size"] = strconv.FormatInt(options.Size, 10)
	}

	if options.SnapshotId != "" {
		params["SnapshotId"] = options.SnapshotId
	}

	if options.VolumeType != "" {
		params["VolumeType"] = options.VolumeType
	}

	if options.IOPS > 0 {
		params["Iops"] = strconv.FormatInt(options.IOPS, 10)
	}

	resp = &CreateVolumeResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// Delete an EBS volume.
func (ec2 *EC2) DeleteVolume(id string) (resp *SimpleResp, err error) {
	params := makeParams("DeleteVolume")
	params["VolumeId"] = id

	resp = &SimpleResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}
	return
}

// Detaches an EBS volume.
func (ec2 *EC2) DetachVolume(id string) (resp *SimpleResp, err error) {
	params := makeParams("DetachVolume")
	params["VolumeId"] = id

	resp = &SimpleResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}
	return
}

// Finds or lists all volumes.
func (ec2 *EC2) Volumes(volIds []string, filter *Filter) (resp *VolumesResp, err error) {
	params := makeParams("DescribeVolumes")
	addParamsList(params, "VolumeId", volIds)
	filter.addParams(params)
	resp = &VolumesResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}
	return
}

// ----------------------------------------------------------------------------
// Security group management functions and types.

// SimpleResp represents a response to an EC2 request which on success will
// return no other information besides a request id.
type SimpleResp struct {
	XMLName   xml.Name
	RequestId string `xml:"requestId"`
}

// CreateSecurityGroupResp represents a response to a CreateSecurityGroup request.
type CreateSecurityGroupResp struct {
	SecurityGroup
	RequestId string `xml:"requestId"`
}

// CreateSecurityGroup run a CreateSecurityGroup request in EC2, with the provided
// name and description.
//
// See http://goo.gl/Eo7Yl for more details.
func (ec2 *EC2) CreateSecurityGroup(group SecurityGroup) (resp *CreateSecurityGroupResp, err error) {
	params := makeParams("CreateSecurityGroup")
	params["GroupName"] = group.Name
	params["GroupDescription"] = group.Description
	if group.VpcId != "" {
		params["VpcId"] = group.VpcId
	}

	resp = &CreateSecurityGroupResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}
	resp.Name = group.Name
	return resp, nil
}

// SecurityGroupsResp represents a response to a DescribeSecurityGroups
// request in EC2.
//
// See http://goo.gl/k12Uy for more details.
type SecurityGroupsResp struct {
	RequestId string              `xml:"requestId"`
	Groups    []SecurityGroupInfo `xml:"securityGroupInfo>item"`
}

// SecurityGroup encapsulates details for a security group in EC2.
//
// See http://goo.gl/CIdyP for more details.
type SecurityGroupInfo struct {
	SecurityGroup
	OwnerId       string   `xml:"ownerId"`
	Description   string   `xml:"groupDescription"`
	IPPerms       []IPPerm `xml:"ipPermissions>item"`
	IPPermsEgress []IPPerm `xml:"ipPermissionsEgress>item"`
}

// IPPerm represents an allowance within an EC2 security group.
//
// See http://goo.gl/4oTxv for more details.
type IPPerm struct {
	Protocol     string              `xml:"ipProtocol"`
	FromPort     int                 `xml:"fromPort"`
	ToPort       int                 `xml:"toPort"`
	SourceIPs    []string            `xml:"ipRanges>item>cidrIp"`
	SourceGroups []UserSecurityGroup `xml:"groups>item"`
}

// UserSecurityGroup holds a security group and the owner
// of that group.
type UserSecurityGroup struct {
	Id      string `xml:"groupId"`
	Name    string `xml:"groupName"`
	OwnerId string `xml:"userId"`
}

// SecurityGroup represents an EC2 security group.
// If SecurityGroup is used as a parameter, then one of Id or Name
// may be empty. If both are set, then Id is used.
type SecurityGroup struct {
	Id          string `xml:"groupId"`
	Name        string `xml:"groupName"`
	Description string `xml:"groupDescription"`
	VpcId       string `xml:"vpcId"`
}

// SecurityGroupNames is a convenience function that
// returns a slice of security groups with the given names.
func SecurityGroupNames(names ...string) []SecurityGroup {
	g := make([]SecurityGroup, len(names))
	for i, name := range names {
		g[i] = SecurityGroup{Name: name}
	}
	return g
}

// SecurityGroupNames is a convenience function that
// returns a slice of security groups with the given ids.
func SecurityGroupIds(ids ...string) []SecurityGroup {
	g := make([]SecurityGroup, len(ids))
	for i, id := range ids {
		g[i] = SecurityGroup{Id: id}
	}
	return g
}

// SecurityGroups returns details about security groups in EC2.  Both parameters
// are optional, and if provided will limit the security groups returned to those
// matching the given groups or filtering rules.
//
// See http://goo.gl/k12Uy for more details.
func (ec2 *EC2) SecurityGroups(groups []SecurityGroup, filter *Filter) (resp *SecurityGroupsResp, err error) {
	params := makeParams("DescribeSecurityGroups")
	i, j := 1, 1
	for _, g := range groups {
		if g.Id != "" {
			params["GroupId."+strconv.Itoa(i)] = g.Id
			i++
		} else {
			params["GroupName."+strconv.Itoa(j)] = g.Name
			j++
		}
	}
	filter.addParams(params)

	resp = &SecurityGroupsResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// DeleteSecurityGroup removes the given security group in EC2.
//
// See http://goo.gl/QJJDO for more details.
func (ec2 *EC2) DeleteSecurityGroup(group SecurityGroup) (resp *SimpleResp, err error) {
	params := makeParams("DeleteSecurityGroup")
	if group.Id != "" {
		params["GroupId"] = group.Id
	} else {
		params["GroupName"] = group.Name
	}

	resp = &SimpleResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// AuthorizeSecurityGroup creates an allowance for clients matching the provided
// rules to access instances within the given security group.
//
// See http://goo.gl/u2sDJ for more details.
func (ec2 *EC2) AuthorizeSecurityGroup(group SecurityGroup, perms []IPPerm) (resp *SimpleResp, err error) {
	return ec2.authOrRevoke("AuthorizeSecurityGroupIngress", group, perms)
}

// RevokeSecurityGroup revokes permissions from a group.
//
// See http://goo.gl/ZgdxA for more details.
func (ec2 *EC2) RevokeSecurityGroup(group SecurityGroup, perms []IPPerm) (resp *SimpleResp, err error) {
	return ec2.authOrRevoke("RevokeSecurityGroupIngress", group, perms)
}

// AuthorizeSecurityGroupEgress creates an allowance for instances within the
// given security group to access servers matching the provided rules.
//
// See http://goo.gl/R91LXY for more details.
func (ec2 *EC2) AuthorizeSecurityGroupEgress(group SecurityGroup, perms []IPPerm) (resp *SimpleResp, err error) {
	return ec2.authOrRevoke("AuthorizeSecurityGroupEgress", group, perms)
}

// RevokeSecurityGroupEgress revokes egress permissions from a group
//
// see http://goo.gl/Zv4wh8
func (ec2 *EC2) RevokeSecurityGroupEgress(group SecurityGroup, perms []IPPerm) (resp *SimpleResp, err error) {
	return ec2.authOrRevoke("RevokeSecurityGroupEgress", group, perms)
}

func (ec2 *EC2) authOrRevoke(op string, group SecurityGroup, perms []IPPerm) (resp *SimpleResp, err error) {
	params := makeParams(op)
	if group.Id != "" {
		params["GroupId"] = group.Id
	} else {
		params["GroupName"] = group.Name
	}

	for i, perm := range perms {
		prefix := "IpPermissions." + strconv.Itoa(i+1)
		params[prefix+".IpProtocol"] = perm.Protocol
		params[prefix+".FromPort"] = strconv.Itoa(perm.FromPort)
		params[prefix+".ToPort"] = strconv.Itoa(perm.ToPort)
		for j, ip := range perm.SourceIPs {
			params[prefix+".IpRanges."+strconv.Itoa(j+1)+".CidrIp"] = ip
		}
		for j, g := range perm.SourceGroups {
			subprefix := prefix + ".Groups." + strconv.Itoa(j+1)
			if g.OwnerId != "" {
				params[subprefix+".UserId"] = g.OwnerId
			}
			if g.Id != "" {
				params[subprefix+".GroupId"] = g.Id
			} else {
				params[subprefix+".GroupName"] = g.Name
			}
		}
	}

	resp = &SimpleResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// ----------------------------------------------------------------------------
// Elastic IP management functions and types.

// Response to a DescribeAddresses request.
//
// See http://goo.gl/zW7J4p for more details.
type DescribeAddressesResp struct {
	RequestId string    `xml:"requestId"`
	Addresses []Address `xml:"addressesSet>item"`
}

// Address represents an Elastic IP Address
// See http://goo.gl/uxCjp7 for more details
type Address struct {
	PublicIp                string `xml:"publicIp"`
	AllocationId            string `xml:"allocationId"`
	Domain                  string `xml:"domain"`
	InstanceId              string `xml:"instanceId"`
	AssociationId           string `xml:"associationId"`
	NetworkInterfaceId      string `xml:"networkInterfaceId"`
	NetworkInterfaceOwnerId string `xml:"networkInterfaceOwnerId"`
	PrivateIpAddress        string `xml:"privateIpAddress"`
}

// DescribeAddresses returns details about one or more
// Elastic IP Addresses. Returned addresses can be
// filtered by Public IP, Allocation ID or multiple filters
//
// See http://goo.gl/zW7J4p for more details.
func (ec2 *EC2) DescribeAddresses(publicIps []string, allocationIds []string, filter *Filter) (resp *DescribeAddressesResp, err error) {
	params := makeParams("DescribeAddresses")
	addParamsList(params, "PublicIp", publicIps)
	addParamsList(params, "AllocationId", allocationIds)
	filter.addParams(params)
	resp = &DescribeAddressesResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}
	return
}

// AllocateAddressOptions are request parameters for allocating an Elastic IP Address
//
// See http://docs.aws.amazon.com/AWSEC2/latest/APIReference/ApiReference-query-AllocateAddress.html
type AllocateAddressOptions struct {
	Domain string
}

// Response to an AllocateAddress request
//
// See http://goo.gl/aLPmbm for more details
type AllocateAddressResp struct {
	RequestId    string `xml:"requestId"`
	PublicIp     string `xml:"publicIp"`
	Domain       string `xml:"domain"`
	AllocationId string `xml:"allocationId"`
}

// Allocates a new Elastic IP address.
// The domain parameter is optional and is used for provisioning an ip address
// in EC2 or in VPC respectively
//
// See http://goo.gl/aLPmbm for more details
func (ec2 *EC2) AllocateAddress(options *AllocateAddressOptions) (resp *AllocateAddressResp, err error) {
	params := makeParams("AllocateAddress")
	params["Domain"] = options.Domain
	resp = &AllocateAddressResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// Response to a ReleaseAddress request
//
// See http://goo.gl/Ciw2Z8 for more details
type ReleaseAddressResp struct {
	RequestId string `xml:"requestId"`
	Return    bool   `xml:"return"`
}

// Release existing elastic ip address from the account
// PublicIp = Required for EC2
// AllocationId = Required for VPC
//
// See http://goo.gl/Ciw2Z8 for more details
func (ec2 *EC2) ReleaseAddress(publicIp, allocationId string) (resp *ReleaseAddressResp, err error) {
	params := makeParams("ReleaseAddress")

	if publicIp != "" {
		params["PublicIp"] = publicIp

	}
	if allocationId != "" {
		params["AllocationId"] = allocationId
	}

	resp = &ReleaseAddressResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// Options set for AssociateAddress
//
// See http://goo.gl/hhj4z7 for more details
type AssociateAddressOptions struct {
	PublicIp           string
	InstanceId         string
	AllocationId       string
	NetworkInterfaceId string
	PrivateIpAddress   string
	AllowReassociation bool
}

// Response to an AssociateAddress request
//
// See http://goo.gl/hhj4z7 for more details
type AssociateAddressResp struct {
	RequestId     string `xml:"requestId"`
	Return        bool   `xml:"return"`
	AssociationId string `xml:"associationId"`
}

// Associate an Elastic ip address to an instance id or a network interface
//
// See http://goo.gl/hhj4z7 for more details
func (ec2 *EC2) AssociateAddress(options *AssociateAddressOptions) (resp *AssociateAddressResp, err error) {
	params := makeParams("AssociateAddress")
	params["InstanceId"] = options.InstanceId
	if options.PublicIp != "" {
		params["PublicIp"] = options.PublicIp
	}
	if options.AllocationId != "" {
		params["AllocationId"] = options.AllocationId
	}
	if options.NetworkInterfaceId != "" {
		params["NetworkInterfaceId"] = options.NetworkInterfaceId
	}
	if options.PrivateIpAddress != "" {
		params["PrivateIpAddress"] = options.PrivateIpAddress
	}
	if options.AllowReassociation {
		params["AllowReassociation"] = "true"
	}

	resp = &AssociateAddressResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// Response to a Disassociate Address request
//
// See http://goo.gl/Dapkuzfor more details
type DisassociateAddressResp struct {
	RequestId string `xml:"requestId"`
	Return    bool   `xml:"return"`
}

// Disassociate an elastic ip address from an instance
// PublicIp - Required for EC2
// AssociationId - Required for VPC
// See http://goo.gl/Dapkuz for more details
func (ec2 *EC2) DisassociateAddress(publicIp, associationId string) (resp *DisassociateAddressResp, err error) {
	params := makeParams("DisassociateAddress")
	if publicIp != "" {
		params["PublicIp"] = publicIp
	}
	if associationId != "" {
		params["AssociationId"] = associationId
	}

	resp = &DisassociateAddressResp{}
	err = ec2.query(params, resp)
	if err != nil {
		return nil, err
	}
	return resp, nil
}
