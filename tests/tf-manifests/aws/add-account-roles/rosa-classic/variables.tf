variable "rhcs_environment" {
  type    = string
  default = "staging"
}

variable "openshift_version" {
  type    = string
  default = "4.13.13"
}

variable "account_role_prefix" {
  type    = string
  default = ""
}

variable "shared_vpc_role_arn" {
  type    = string
  default = null
}

variable "channel_group" {
  type    = string
  default = "stable"
}

variable "path" {
  description = "(Optional) The arn path for the account/operator roles as well as their policies."
  type        = string
  default     = null
}