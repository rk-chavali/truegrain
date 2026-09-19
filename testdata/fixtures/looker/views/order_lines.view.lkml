view: order_lines {
  sql_table_name: analytics.order_lines ;;

  dimension: line_id {
    primary_key: yes
    type: number
  }

  dimension: order_id {
    type: number
  }

  dimension: sku {
    type: string
  }

  measure: line_revenue {
    type: sum
    sql: ${TABLE}.line_amount ;;
  }

  measure: units {
    type: sum
    sql: ${TABLE}.quantity ;;
  }
}
