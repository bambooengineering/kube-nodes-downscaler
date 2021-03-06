package main

import (
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/alecthomas/kingpin"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/autoscaling"
)

type autoscalingInterface interface {
	SetDesiredCapacity(input *autoscaling.SetDesiredCapacityInput) (*autoscaling.SetDesiredCapacityOutput, error)
	DescribeAutoScalingInstances(input *autoscaling.DescribeAutoScalingInstancesInput) (*autoscaling.DescribeAutoScalingInstancesOutput, error)
	DescribeAutoScalingGroups(input *autoscaling.DescribeAutoScalingGroupsInput) (*autoscaling.DescribeAutoScalingGroupsOutput, error)
}

var errIgnored = errors.New("nothing to worry about")

// ASG is the basic data type to deal with AWS ASGs.
type ASG struct {
	Name   string
	Client autoscalingInterface
}

type downscaler struct {
	startTime      int
	endTime        int
	lastASGSize    int
	interval       time.Duration
	consultantMode bool
	debug          bool
	asg            *ASG
}

// SetCapacity sets the capacity of the ASG to "capacity"
func (a *ASG) SetCapacity(capacity int64) error {
	input := &autoscaling.SetDesiredCapacityInput{
		AutoScalingGroupName: aws.String(a.Name),
		DesiredCapacity:      aws.Int64(capacity),
		HonorCooldown:        aws.Bool(true),
	}

	_, err := a.Client.SetDesiredCapacity(input)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case autoscaling.ErrCodeScalingActivityInProgressFault:
				log.Printf("cannot autoscale due to activity in progress: %v\n", aerr.Error())
				return errIgnored
			case autoscaling.ErrCodeResourceContentionFault:
				log.Printf("cannot autoscale due to contention: %v\n", aerr.Error())
				return errIgnored
			default:
				return aerr
			}
		}
		return err
	}
	return nil
}

// GetCurrentCapacity fetches the current capacity of the ASG given its name.
func (a *ASG) GetCurrentCapacity() (int, error) {
	out, err := a.Client.DescribeAutoScalingGroups(&autoscaling.DescribeAutoScalingGroupsInput{AutoScalingGroupNames: []*string{aws.String(a.Name)}})
	if err != nil {
		return -1, fmt.Errorf("cannot get current size of autoscaling group: %v", err)
	}
	return int(*out.AutoScalingGroups[0].DesiredCapacity), nil
}

func autodetectASGName(client autoscalingInterface, instanceName *string) (string, error) {
	out, err := client.DescribeAutoScalingInstances(&autoscaling.DescribeAutoScalingInstancesInput{InstanceIds: []*string{
		instanceName,
	}})
	if err != nil {
		return "", err
	}
	instances := out.AutoScalingInstances
	if len(instances) != 1 {
		return "", fmt.Errorf("wrong size of autoscaling instances, expected 1, have %d", len(instances))
	}
	return *instances[0].AutoScalingGroupName, nil
}

func determineNewCapacity(startTime, endTime, previousCap, maxCap int, day time.Weekday, currentHour int, consultantMode bool) int {
	if currentHour > endTime || currentHour < startTime {
		return 0
	}
	if day == time.Saturday || day == time.Sunday {
		if consultantMode {
			if currentHour >= startTime {
				return maxCap
			}
		} else {
			return 0
		}
	} else {
		if currentHour >= startTime {
			return maxCap
		}
	}
	return previousCap
}

func updateCapacity(cap, newCap int, asg *ASG) error {
	if newCap != cap {
		err := asg.SetCapacity(int64(newCap))
		if err != nil {
			if err == errIgnored {
				return errIgnored
			}
			return fmt.Errorf("error setting ASG capacity: %v", err)
		}
	}
	return nil
}

func validateParams(startTime, endTime int) error {
	if startTime < 1 || startTime > 24 {
		return fmt.Errorf("start of working day should be greater or equal than 1 and less than 24, have: %d", startTime)
	}
	if endTime < 1 || endTime > 24 {
		return fmt.Errorf("end of working day should be greater or equal than 1 and less than 24, have: %d", endTime)
	}

	if endTime < startTime {
		return fmt.Errorf("end of working day %d should be greater than start %d", endTime, startTime)
	}
	return nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (d *downscaler) do(t *time.Time) {
	day := t.Weekday()
	cap, err := d.asg.GetCurrentCapacity()
	if err != nil {
		log.Fatalf("error getting current ASG capacity: %v", err)
	}
	newCap := determineNewCapacity(d.startTime, d.endTime, cap, max(cap, d.lastASGSize), day, t.Hour(), d.consultantMode)
	log.Printf("At %d determined capacity to be %d", t.Hour(), newCap)
	err = updateCapacity(cap, newCap, d.asg)
	if err != nil && err != errIgnored {
		log.Fatal(err)
	}
	if err == nil {
		d.lastASGSize = max(newCap, d.lastASGSize)
	}
	if d.debug {
		log.Printf("Nothing left to do, going to sleep for %v seconds\n", d.interval)
	}
}

func main() {
	startTime := kingpin.Flag("start", "Start of the working day. 24h format.").Default("9").Int()
	endTime := kingpin.Flag("end", "End of the working day. 24h format.").Default("18").Int()
	consultantMode := kingpin.Flag("consultant-mode", "When true, will make sure that the nodes are available during the weekend.").Default("false").Bool()
	asgName := kingpin.Flag("asg-name", "Name of the autoscaling group. Useful to make the downscaler handle different ASGs from the one it's running on.").String()
	autoDetectASG := kingpin.Flag("autodetect", "Autodetect ASG group name, which is the ASG where this application is running.").Bool()
	interval := kingpin.Flag("interval", "Interval by which the size is checked.").Default("60s").Duration()
	debug := kingpin.Flag("verbose", "Enables verbose logging").Default("false").Bool()
	lastASGSize := kingpin.Flag("initial-asg-size", "Initial size of the ASG.").Default("3").Int()
	kingpin.Parse()

	session := session.New()

	err := validateParams(*startTime, *endTime)
	if err != nil {
		log.Fatalf("invalid params: %v", err)
	}

	svc := ec2metadata.New(session)
	id, err := svc.GetInstanceIdentityDocument()
	region := id.Region
	if err != nil {
		log.Fatalf("Cannot get identity document: %v\n", err)
	}
	client := autoscaling.New(session, aws.NewConfig().WithRegion(region))
	if *autoDetectASG == true {
		asg, err := autodetectASGName(client, &id.InstanceID)
		if err != nil {
			log.Fatalf("Cannot get ASG name: %v\n", err)
		}
		*asgName = asg
	}

	if *asgName == "" {
		log.Fatalf("No ASG name provided, exiting.\n")
	}

	asg := ASG{
		Name:   *asgName,
		Client: client,
	}

	log.Println("starting the loop")
	d := &downscaler{
		startTime:      *startTime,
		endTime:        *endTime,
		interval:       *interval,
		debug:          *debug,
		lastASGSize:    *lastASGSize,
		consultantMode: *consultantMode,
		asg:            &asg,
	}
	for {
		t := time.Now()
		d.do(&t)
		time.Sleep(d.interval)
	}
}
