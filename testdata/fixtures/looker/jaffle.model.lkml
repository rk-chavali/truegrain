connection: "warehouse"
include: "/views/*.view.lkml"

explore: orders {
  # The join that matters. one_to_many means each order matches many lines,
  # so summing an order-grain measure across it inflates the total. Looker
  # handles this with symmetric aggregates and says nothing; this engine
  # refuses and says why.
  join: order_lines {
    relationship: one_to_many
    sql_on: ${orders.order_id} = ${order_lines.order_id} ;;
  }

  join: customers {
    relationship: many_to_one
    sql_on: ${orders.customer_id} = ${customers.customer_id} ;;
  }
}

explore: order_lines {
  # The same join declared from the other side. It must not produce a
  # second, contradictory relationship.
  join: orders {
    relationship: many_to_one
    sql_on: ${order_lines.order_id} = ${orders.order_id} ;;
  }
}

explore: web {
  from: customers

  # Neither side unique. There is no key to declare, so the planner could
  # check nothing.
  join: order_lines {
    relationship: many_to_many
    sql_on: ${customers.region} = ${order_lines.sku} ;;
  }

  # An expression, not a column equality.
  join: orders {
    relationship: many_to_one
    sql_on: LOWER(${customers.region}) = ${orders.status} ;;
  }
}
