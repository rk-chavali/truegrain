view: customers {
  sql_table_name: analytics.customers ;;

  dimension: customer_id {
    primary_key: yes
    type: number
  }

  dimension: region {
    type: string
  }

  measure: customer_count {
    type: count_distinct
    sql: ${TABLE}.customer_id ;;
  }

  # No portable ANSI form.
  measure: median_spend {
    type: median
    sql: ${TABLE}.lifetime_value ;;
  }
}
