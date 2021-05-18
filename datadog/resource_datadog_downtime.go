package datadog

import (
	"context"
	"fmt"
	"log"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/terraform-providers/terraform-provider-datadog/datadog/internal/utils"
	"github.com/terraform-providers/terraform-provider-datadog/datadog/internal/validators"

	// embed time zone data
	_ "time/tzdata"

	datadogV1 "github.com/DataDog/datadog-api-client-go/api/v1/datadog"
	"github.com/hashicorp/terraform-plugin-sdk/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/helper/validation"
)

func resourceDatadogDowntime() *schema.Resource {
	return &schema.Resource{
		Description: "Provides a Datadog downtime resource. This can be used to create and manage Datadog downtimes.",
		Create:      resourceDatadogDowntimeCreate,
		Read:        resourceDatadogDowntimeRead,
		Update:      resourceDatadogDowntimeUpdate,
		Delete:      resourceDatadogDowntimeDelete,
		Importer: &schema.ResourceImporter{
			State: resourceDatadogDowntimeImport,
		},

		Schema: map[string]*schema.Schema{
			"active": {
				Type:        schema.TypeBool,
				Computed:    true,
				Description: "When true indicates this downtime is being actively applied",
			},
			"disabled": {
				Type:        schema.TypeBool,
				Computed:    true,
				Description: "When true indicates this downtime is not being applied",
			},
			"start": {
				Type:     schema.TypeInt,
				Optional: true,
				DiffSuppressFunc: func(k, oldVal, newVal string, d *schema.ResourceData) bool {
					_, startDatePresent := d.GetOk("start_date")
					now := time.Now().Unix()
					// If "start_date" is set, ignore diff for "start". If "start" isn't set, ignore diff if start is now or in the past
					return startDatePresent || (newVal == "0" && oldVal != "0" && int64(d.Get("start").(int)) <= now)
				},
				Description: "Specify when this downtime should start",
			},
			"start_date": {
				Type:          schema.TypeString,
				ValidateFunc:  validation.IsRFC3339Time,
				ConflictsWith: []string{"start"},
				Optional:      true,
				Description:   "String representing date and time to start the downtime in RFC3339 format.",
			},
			"end": {
				Type:     schema.TypeInt,
				Optional: true,
				DiffSuppressFunc: func(k, oldVal, newVal string, d *schema.ResourceData) bool {
					_, endDatePresent := d.GetOk("end_date")
					return endDatePresent
				},
				Description: "Optionally specify an end date when this downtime should expire",
			},
			"end_date": {
				Type:          schema.TypeString,
				ValidateFunc:  validation.IsRFC3339Time,
				ConflictsWith: []string{"end"},
				Optional:      true,
				Description:   "String representing date and time to end the downtime in RFC3339 format.",
			},
			"timezone": {
				Type:         schema.TypeString,
				Default:      "UTC",
				Optional:     true,
				Description:  "The timezone for the downtime, default UTC",
				ValidateFunc: validators.ValidateDatadogDowntimeTimezone,
			},
			"message": {
				Type:        schema.TypeString,
				Optional:    true,
				Description: "An optional message to provide when creating the downtime, can include notification handles",
				StateFunc: func(val interface{}) string {
					return strings.TrimSpace(val.(string))
				},
			},
			"recurrence": {
				Type:        schema.TypeList,
				Optional:    true,
				MaxItems:    1,
				Description: "Optional recurring schedule for this downtime",
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"period": {
							Type:        schema.TypeInt,
							Optional:    true,
							Description: "How often to repeat as an integer. For example to repeat every 3 days, select a `type` of `days` and a `period` of `3`.",
						},
						"type": {
							Type:         schema.TypeString,
							Required:     true,
							ValidateFunc: validators.ValidateDatadogDowntimeRecurrenceType,
							Description:  "One of `days`, `weeks`, `months`, or `years`",
						},
						"until_date": {
							Type:          schema.TypeInt,
							Optional:      true,
							ConflictsWith: []string{"recurrence.until_occurrences"},
							Description:   "The date at which the recurrence should end as a POSIX timestamp. `until_occurrences` and `until_date` are mutually exclusive.",
						},
						"until_occurrences": {
							Type:          schema.TypeInt,
							Optional:      true,
							ConflictsWith: []string{"recurrence.until_date"},
							Description:   "How many times the downtime will be rescheduled. `until_occurrences` and `until_date` are mutually exclusive.",
						},
						"week_days": {
							Type:        schema.TypeList,
							Optional:    true,
							Description: "A list of week days to repeat on. Choose from: `Mon`, `Tue`, `Wed`, `Thu`, `Fri`, `Sat` or `Sun`. Only applicable when `type` is `weeks`. First letter must be capitalized.",
							Elem: &schema.Schema{
								Type:         schema.TypeString,
								ValidateFunc: validators.ValidateDatadogDowntimeRecurrenceWeekDays,
							},
						},
						"rrule": {
							Description:   "The RRULE standard for defining recurring events. For example, to have a recurring event on the first day of each month, use `FREQ=MONTHLY;INTERVAL=1`. Most common rrule options from the iCalendar Spec are supported. Attributes specifying the duration in RRULE are not supported (for example, `DTSTART`, `DTEND`, `DURATION`).",
							Type:          schema.TypeString,
							Optional:      true,
							ConflictsWith: []string{"recurrence.period", "recurrence.until_date", "recurrence.until_occurrences", "recurrence.week_days"},
						},
					},
				},
			},
			"scope": {
				Type:        schema.TypeList,
				Required:    true,
				Elem:        &schema.Schema{Type: schema.TypeString},
				Description: "specify the group scope to which this downtime applies. For everything use '*'",
			},
			"monitor_id": {
				Type:          schema.TypeInt,
				Optional:      true,
				ConflictsWith: []string{"monitor_tags"},
				Description:   "When specified, this downtime will only apply to this monitor",
			},
			"monitor_tags": {
				// TypeSet makes Terraform ignore differences in order when creating a plan
				Type:        schema.TypeSet,
				Optional:    true,
				Description: "A list of monitor tags (up to 25), i.e. tags that are applied directly to monitors to which the downtime applies",
				// MonitorTags conflicts with MonitorId and it also has a default of `["*"]`, which brings some problems:
				// * We can't use DefaultFunc to default to ["*"], since that's incompatible with
				//   ConflictsWith
				// * Since this is a TypeSet, DiffSuppressFunc can't really be written well for it
				//   (it is called and expected to give result for each element, not for the whole
				//    list, so there's no way to tell in each iteration whether the new config value
				//    is an empty list).
				// Therefore we handle the "default" manually in resourceDatadogDowntimeRead function
				ConflictsWith: []string{"monitor_id"},
				Elem:          &schema.Schema{Type: schema.TypeString},
			},
		},
	}
}

// getDowntimeBoundaryTimestamp returns an int timestamp for start/end and the name of the
// attribute which it was extracted from (e.g. "end" or "end_date"). Arguments:
// * `d` - current `*schema.ResourceData`
// * `dateAttr` - name of the attribute in `d` which carries and RFC3339 date string, e.g. `end_date`
// * `tsAttr` - name of the attribute in `d` which carries integer timestamp, e.g. `end`
func getDowntimeBoundaryTimestamp(d *schema.ResourceData, dateAttr, tsAttr string) (ts int64, tsFrom string) {
	if attr, ok := d.GetOk(dateAttr); ok {
		if t, err := time.Parse(time.RFC3339, attr.(string)); err == nil {
			tsFrom = dateAttr
			ts = t.Unix()
		}
	} else if attr, ok := d.GetOk(tsAttr); ok {
		tsFrom = tsAttr
		ts = int64(attr.(int))
	}
	return ts, tsFrom
}

// downtimeBoundaryNeedsApply returns a boolean value signifying whether or not the boundary (start/end)
// should be included in the API POST/PUT request. Arguments:
// * `d` - current `*schema.ResourceData`
// * `tsFrom` - name of the attribute in `d` from which `configTs` was extracted
// * `apiTs` - current value (returned by API) of the boundary
// * `configTs` - desired value (from TF configuration) of the boundary
// * `updating` - `true` if this call is from Update method of the downtime resource, `false` if from Create
func downtimeBoundaryNeedsApply(d *schema.ResourceData, tsFrom string, apiTs, configTs int64, updating bool) (apply bool) {
	if tsFrom == "" {
		// if the boundary was not specified in the config, don't apply it
		return apply
	}

	if updating {
		// when updating, we apply when
		// * API-returned value is different than configured value
		// * the config value has changed
		if apiTs != configTs || d.HasChange(tsFrom) {
			apply = true
		}
	} else {
		// when creating, we always apply
		apply = true
	}

	return apply
}

func buildDowntimeStruct(ctx context.Context, d *schema.ResourceData, client *datadogV1.APIClient, providerConf *ProviderConfiguration, updating bool) (*datadogV1.Downtime, error) {
	// NOTE: for each of start/start_date/end/end_date, we only send the value when
	// it has changed or if the configured value is different than current value
	// (IOW there's a resource drift). This allows users to change other attributes
	// (e.g. scopes/message/...) without having to update the timestamps/dates to be
	// in the future (this works thanks to the downtime API allowing not to send these
	// values when they shouldn't be touched).
	var dt datadogV1.Downtime
	var currentStart = *datadogV1.PtrInt64(0)
	var currentEnd = *datadogV1.PtrInt64(0)
	if updating {
		id, err := strconv.ParseInt(d.Id(), 10, 64)
		if err != nil {
			return nil, err
		}

		var currdt datadogV1.Downtime
		currdt, httpresp, err := client.DowntimesApi.GetDowntime(ctx, id)
		if err != nil {
			return nil, utils.TranslateClientError(err, httpresp.Request.URL.Host, "error getting downtime")
		}
		currentStart = currdt.GetStart()
		currentEnd = currdt.GetEnd()
	}

	endValue, endAttrName := getDowntimeBoundaryTimestamp(d, "end_date", "end")
	if downtimeBoundaryNeedsApply(d, endAttrName, currentEnd, endValue, updating) {
		dt.SetEnd(endValue)
	}

	if attr, ok := d.GetOk("message"); ok {
		dt.SetMessage(strings.TrimSpace(attr.(string)))
	}
	if attr, ok := d.GetOk("monitor_id"); ok {
		dt.SetMonitorId(int64(attr.(int)))
	}
	if _, ok := d.GetOk("recurrence"); ok {
		var recurrence datadogV1.DowntimeRecurrence

		if attr, ok := d.GetOk("recurrence.0.period"); ok {
			recurrence.SetPeriod(int32(attr.(int)))
		}
		if attr, ok := d.GetOk("recurrence.0.type"); ok {
			recurrence.SetType(attr.(string))
		}
		if attr, ok := d.GetOk("recurrence.0.until_date"); ok {
			recurrence.SetUntilDate(int64(attr.(int)))
		}
		if attr, ok := d.GetOk("recurrence.0.until_occurrences"); ok {
			recurrence.SetUntilOccurrences(int32(attr.(int)))
		}
		if attr, ok := d.GetOk("recurrence.0.week_days"); ok {
			weekDays := make([]string, 0, len(attr.([]interface{})))
			for _, weekDay := range attr.([]interface{}) {
				weekDays = append(weekDays, weekDay.(string))
			}
			recurrence.SetWeekDays(weekDays)
		}
		if attr, ok := d.GetOk("recurrence.0.rrule"); ok {
			recurrence.SetRrule(attr.(string))
		}

		dt.SetRecurrence(recurrence)
	}
	var scope []string
	for _, s := range d.Get("scope").([]interface{}) {
		scope = append(scope, s.(string))
	}
	dt.SetScope(scope)
	var tags []string
	for _, mt := range d.Get("monitor_tags").(*schema.Set).List() {
		tags = append(tags, mt.(string))
	}
	dt.SetMonitorTags(tags)

	startValue, startAttrName := getDowntimeBoundaryTimestamp(d, "start_date", "start")
	if downtimeBoundaryNeedsApply(d, startAttrName, currentStart, startValue, updating) {
		dt.SetStart(startValue)
	}

	if attr, ok := d.GetOk("timezone"); ok {
		dt.SetTimezone(attr.(string))
	}

	return &dt, nil
}

func resourceDatadogDowntimeCreate(d *schema.ResourceData, meta interface{}) error {
	providerConf := meta.(*ProviderConfiguration)
	datadogClientV1 := providerConf.DatadogClientV1
	authV1 := providerConf.AuthV1

	dts, err := buildDowntimeStruct(authV1, d, datadogClientV1, providerConf, false)
	if err != nil {
		return fmt.Errorf("failed to parse resource configuration: %s", err.Error())
	}
	dt, httpresp, err := datadogClientV1.DowntimesApi.CreateDowntime(authV1, *dts)
	if err != nil {
		return utils.TranslateClientError(err, httpresp.Request.URL.Host, "error creating downtime")
	}

	d.SetId(strconv.Itoa(int(dt.GetId())))

	return updateDowntimeState(d, &dt)
}

func resourceDatadogDowntimeRead(d *schema.ResourceData, meta interface{}) error {
	providerConf := meta.(*ProviderConfiguration)
	datadogClientV1 := providerConf.DatadogClientV1
	authV1 := providerConf.AuthV1

	id, err := strconv.ParseInt(d.Id(), 10, 64)
	if err != nil {
		return err
	}

	dt, httpresp, err := datadogClientV1.DowntimesApi.GetDowntime(authV1, id)
	if err != nil {
		if httpresp != nil && httpresp.StatusCode == 404 {
			d.SetId("")
			return nil
		}
		return utils.TranslateClientError(err, httpresp.Request.URL.Host, "error getting downtime")
	}

	if canceled, ok := dt.GetCanceledOk(); ok && canceled != nil {
		d.SetId("")
		return nil
	}

	return updateDowntimeState(d, &dt)
}

func updateDowntimeState(d *schema.ResourceData, dt *datadogV1.Downtime) error {
	log.Printf("[DEBUG] downtime: %v", dt)

	if err := d.Set("active", dt.GetActive()); err != nil {
		return err
	}
	if err := d.Set("disabled", dt.GetDisabled()); err != nil {
		return err
	}
	if err := d.Set("end", dt.GetEnd()); err != nil {
		return err
	}
	if err := d.Set("message", dt.GetMessage()); err != nil {
		return err
	}
	if v, ok := dt.GetMonitorIdOk(); ok && v != nil {
		if err := d.Set("monitor_id", v); err != nil {
			return err
		}
	}
	if err := d.Set("timezone", dt.GetTimezone()); err != nil {
		return err
	}

	if r, ok := dt.GetRecurrenceOk(); ok && r != nil {
		recurrence := make(map[string]interface{})
		recurrenceList := make([]map[string]interface{}, 0, 1)

		if attr, ok := r.GetPeriodOk(); ok {
			recurrence["period"] = attr
		}
		if attr, ok := r.GetTypeOk(); ok {
			recurrence["type"] = attr
		}
		if attr, ok := r.GetUntilDateOk(); ok {
			recurrence["until_date"] = attr
		}
		if attr, ok := r.GetUntilOccurrencesOk(); ok {
			recurrence["until_occurrences"] = attr
		}
		if r.GetWeekDays() != nil {
			weekDays := make([]string, 0, len(r.GetWeekDays()))
			for _, weekDay := range *r.WeekDays {
				weekDays = append(weekDays, weekDay)
			}
			recurrence["week_days"] = weekDays
		}
		if attr, ok := r.GetRruleOk(); ok {
			recurrence["rrule"] = attr
		}
		recurrenceList = append(recurrenceList, recurrence)
		if err := d.Set("recurrence", recurrenceList); err != nil {
			return err
		}
	}
	if err := d.Set("scope", dt.Scope); err != nil {
		return err
	}
	// See the comment for monitor_tags in the schema definition above
	if !reflect.DeepEqual(dt.GetMonitorTags(), []string{"*"}) {
		if err := d.Set("monitor_tags", dt.GetMonitorTags()); err != nil {
			return err
		}
	}
	if err := d.Set("start", dt.GetStart()); err != nil {
		return err
	}
	return nil
}

func resourceDatadogDowntimeUpdate(d *schema.ResourceData, meta interface{}) error {
	providerConf := meta.(*ProviderConfiguration)
	datadogClientV1 := providerConf.DatadogClientV1
	authV1 := providerConf.AuthV1

	dt, err := buildDowntimeStruct(authV1, d, datadogClientV1, providerConf, true)
	if err != nil {
		return fmt.Errorf("failed to parse resource configuration: %s", err.Error())
	}
	id, err := strconv.ParseInt(d.Id(), 10, 64)
	if err != nil {
		return err
	}
	// above downtimeStruct returns nil if downtime is not set. Hence, if we are handling the cases where downtime
	// is replaced, the ID of the downtime will be set to 0.
	dt.SetId(id)

	updatedDowntime, httpresp, err := datadogClientV1.DowntimesApi.UpdateDowntime(authV1, id, *dt)
	if err != nil {
		return utils.TranslateClientError(err, httpresp.Request.URL.Host, "error updating downtime")
	}
	// handle the case when a downtime is replaced
	d.SetId(strconv.FormatInt(dt.GetId(), 10))

	return updateDowntimeState(d, &updatedDowntime)
}

func resourceDatadogDowntimeDelete(d *schema.ResourceData, meta interface{}) error {
	providerConf := meta.(*ProviderConfiguration)
	datadogClientV1 := providerConf.DatadogClientV1
	authV1 := providerConf.AuthV1

	id, err := strconv.ParseInt(d.Id(), 10, 64)
	if err != nil {
		return err
	}

	if httpresp, err := datadogClientV1.DowntimesApi.CancelDowntime(authV1, id); err != nil {
		return utils.TranslateClientError(err, httpresp.Request.URL.Host, "error deleting downtime")
	}

	return nil
}

func resourceDatadogDowntimeImport(d *schema.ResourceData, meta interface{}) ([]*schema.ResourceData, error) {
	if err := resourceDatadogDowntimeRead(d, meta); err != nil {
		return nil, err
	}
	return []*schema.ResourceData{d}, nil
}
