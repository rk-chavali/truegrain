# One row per order. The grain is stated, which is what the engine needs.
view: orders {
  sql_table_name: analytics.orders ;;

  dimension: order_id {
    primary_key: yes
    type: number
    sql: ${TABLE}.order_id ;;
  }

  dimension: customer_id {
    type: number
    sql: ${TABLE}.customer_id ;;
    hidden: yes
  }

  dimension: status {
    type: string
    sql: ${TABLE}.status ;;
    description: "Where the order got to"
  }

  dimension_group: created {
    type: time
    timeframes: [raw, date, week, month, year]
    sql: ${TABLE}.created_at ;;
  }

  # An expression rather than a column. Not importable as a field.
  dimension: is_large {
    type: yesno
    sql: ${TABLE}.amount > 100 ;;
  }

  measure: total_revenue {
    type: sum
    sql: ${TABLE}.amount ;;
    description: "What the business calls revenue"
  }

  measure: count {
    type: count
  }

  # Filtered, so it needs a filtered aggregate to reproduce exactly.
  measure: shipped_revenue {
    type: sum
    sql: ${TABLE}.amount ;;
    filters: [status: "shipped"]
  }

  # A window function over an ordered frame.
  measure: revenue_running {
    type: running_total
    sql: ${TABLE}.amount ;;
  }

  # An expression over other measures, possibly across grains.
  measure: revenue_per_order {
    type: number
    sql: ${total_revenue} / NULLIF(${count}, 0) ;;
  }
}
